package elastic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/json-iterator/go"
	elasticV6 "github.com/olivere/elastic"
	"github.com/robfig/cron"
	elasticV3 "gopkg.in/olivere/elastic.v3"
	elasticV5 "gopkg.in/olivere/elastic.v5"

	"github.com/qiniu/log"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/reader"
	. "github.com/qiniu/logkit/reader/config"
	"github.com/qiniu/logkit/utils"
	. "github.com/qiniu/logkit/utils/models"
)

var (
	_ reader.DaemonReader = &Reader{}
	_ reader.StatsReader  = &Reader{}
	_ reader.Reader       = &Reader{}
	_ Resetable           = &Reader{}
)

const (
	KeyMetaFileName = ".es.log"
)

type Record struct {
	data       json.RawMessage
	cronOffset interface{}
}

type Reader struct {
	meta *reader.Meta // 记录offset的元数据
	// Note: 原子操作，用于表示 reader 整体的运行状态
	status int32
	/*
		Note: 原子操作，用于表示获取数据的线程运行状态

		- StatusInit: 当前没有任务在执行
		- StatusRunning: 当前有任务正在执行
		- StatusStopping: 数据管道已经由上层关闭，执行中的任务完成时直接退出无需再处理
	*/
	routineStatus int32

	stopChan chan struct{}
	readChan chan Record
	errChan  chan error

	stats     StatsInfo
	statsLock sync.RWMutex

	Cron                   *cron.Cron //定时任务
	execOnStart            bool
	isLoop                 bool
	loopDuration           time.Duration
	cronOffsetKey          string
	cronOffsetValue        interface{}
	cronOffsetValueIsValid bool
	metaFile               string
	esindex                string //es索引
	estype                 string //es type
	eshost                 string //eshost+port
	authUsername           string
	authPassword           string
	readBatch              int    // 每次读取的数据量
	keepAlive              string //scrollID 保留时间
	esVersion              string //ElasticSearch version
	offset                 string // 当前处理es的offset
}

func init() {
	reader.RegisterConstructor(ModeElastic, NewReader)
}

func NewReader(meta *reader.Meta, conf conf.MapConf) (reader.Reader, error) {
	readBatch, _ := conf.GetIntOr(KeyESReadBatch, 100)
	estype, err := conf.GetString(KeyESType)
	if err != nil {
		return nil, err
	}
	esindex, err := conf.GetString(KeyESIndex)
	if err != nil {
		return nil, err
	}
	eshost, _ := conf.GetStringOr(KeyESHost, "http://localhost:9200")
	if !strings.HasPrefix(eshost, "http://") && !strings.HasPrefix(eshost, "https://") {
		eshost = "http://" + eshost
	}
	esVersion, _ := conf.GetStringOr(KeyESVersion, ElasticVersion5)
	authUsername, _ := conf.GetStringOr(KeyAuthUsername, "")
	authPassword, _ := conf.GetPasswordEnvStringOr(KeyAuthPassword, "")
	keepAlive, _ := conf.GetStringOr(KeyESKeepAlive, "6h")
	cronSched, _ := conf.GetStringOr(KeyESCron, "")
	execOnStart, _ := conf.GetBoolOr(KeyESExecOnstart, true)
	cronOffset, _ := conf.GetStringOr(KeyESCronOffset, "")
	metaFile := filepath.Join(meta.Dir, KeyMetaFileName)

	offset, _, err := meta.ReadOffset()
	if err != nil {
		log.Errorf("Runner[%v] %v -meta data is corrupted err:%v, omit meta data", meta.RunnerName, meta.MetaFile(), err)
	}
	r := &Reader{
		meta:          meta,
		status:        StatusInit,
		routineStatus: StatusInit,
		stopChan:      make(chan struct{}),
		readChan:      make(chan Record),
		errChan:       make(chan error),
		esindex:       esindex,
		estype:        estype,
		eshost:        eshost,
		authUsername:  authUsername,
		authPassword:  authPassword,
		esVersion:     esVersion,
		readBatch:     readBatch,
		keepAlive:     keepAlive,
		offset:        offset,
		Cron:          cron.New(),
		cronOffsetValueIsValid: false,
		cronOffsetKey:          cronOffset,
		execOnStart:            execOnStart,
		metaFile:               metaFile,
	}
	if len(cronSched) > 0 {
		cronSched = strings.ToLower(cronSched)
		if strings.HasPrefix(cronSched, Loop) {
			r.isLoop = true
			r.loopDuration, err = reader.ParseLoopDuration(cronSched)
			if err != nil {
				log.Errorf("Runner[%v] %v %v", r.meta.RunnerName, r.Name(), err)
			}
			if r.loopDuration.Nanoseconds() <= 0 {
				r.loopDuration = 1 * time.Second
			}
			if utils.IsExist(metaFile) {
				content, err := ioutil.ReadFile(metaFile)
				if err != nil {
					log.Warnf("Runner[%v] %v failed to read offset file[%v]: %v,reset offset and read all data", meta.RunnerName, ModeElastic, metaFile, err)
				} else {
					r.cronOffsetValue = string(content)
					r.cronOffsetValueIsValid = true
				}
			}
		} else {
			err = r.Cron.AddFunc(cronSched, r.run)
			if err != nil {
				return nil, err
			}
			log.Infof("Runner[%v] %v Cron added with schedule <%v>", r.meta.RunnerName, r.Name(), cronSched)
		}
	}
	return r, nil
}

func (r *Reader) isStopping() bool {
	return atomic.LoadInt32(&r.status) == StatusStopping
}

func (r *Reader) hasStopped() bool {
	return atomic.LoadInt32(&r.status) == StatusStopped
}

func (r *Reader) Name() string {
	return "ESReader:" + r.Source()
}

func (r *Reader) run() {
	// 未在准备状态（StatusInit）时无法执行此次任务
	if !atomic.CompareAndSwapInt32(&r.routineStatus, StatusInit, StatusRunning) {
		if r.isStopping() || r.hasStopped() {
			log.Warnf("Runner[%v] %q daemon has stopped, this task does not need to be executed and is skipped this time", r.meta.RunnerName, r.Name())
		} else {
			errMsg := fmt.Sprintf("Runner[%v] %q daemon is still working on last task, this task will not be executed and is skipped this time", r.meta.RunnerName, r.Name())
			log.Error(errMsg)
			if !r.isLoop {
				// 通知上层 Cron 执行间隔可能过短或任务执行时间过长
				r.sendError(errors.New(errMsg))
			}
		}
		return
	}
	defer func() {
		// 如果 reader 在 routine 运行时关闭，则需要此 routine 负责关闭数据管道
		if r.isStopping() || r.hasStopped() {
			if atomic.CompareAndSwapInt32(&r.routineStatus, StatusRunning, StatusStopping) {
				close(r.readChan)
				close(r.errChan)
			}
			return
		}
		atomic.StoreInt32(&r.routineStatus, StatusInit)
	}()

	// 判断上层是否已经关闭，先判断 routineStatus 再判断 status 可以保证同时只有一个 r.run 会运行到此处
	if r.isStopping() || r.hasStopped() {
		log.Warnf("Runner[%v] %q daemon has stopped, task is interrupted", r.meta.RunnerName, r.Name())
		return
	}
	var err error
	if r.isLoop {
		err = r.execWithLoop()
	} else {
		err = r.execWithCron()
	}
	if err == nil {
		log.Infof("Runner[%v] %q task has been successfully executed", r.meta.RunnerName, r.Name())
		return
	}
	r.setStatsError(err.Error())
	r.sendError(err)

	log.Errorf("Runner[%v] %q task execution failed: %v ", r.meta.RunnerName, r.Name(), err)
}

func (r *Reader) SetMode(mode string, v interface{}) error {
	return errors.New("elastic reader not support read mode")
}

func (r *Reader) setStatsError(err string) {
	r.statsLock.Lock()
	defer r.statsLock.Unlock()
	r.stats.LastError = err
}

func (r *Reader) sendError(err error) {
	if err == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf("Reader %q was panicked and recovered from %v", r.Name(), rec)
		}
	}()
	r.errChan <- err
}

func (r *Reader) execWithLoop() error {
	// Create a client
	switch r.esVersion {
	case ElasticVersion6:
		optFns := []elasticV6.ClientOptionFunc{
			elasticV6.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV6.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV6.NewClient(optFns...)
		if err != nil {
			return err
		}
		scroll := client.Scroll(r.esindex).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			ctx := context.Background()
			results, err := scroll.ScrollId(r.offset).Do(ctx)
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				r.readChan <- Record{
					data: *hit.Source,
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	case ElasticVersion3:
		optFns := []elasticV3.ClientOptionFunc{
			elasticV3.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV3.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV3.NewClient(optFns...)
		if err != nil {
			return err
		}
		scroll := client.Scroll(r.esindex).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			results, err := scroll.ScrollId(r.offset).Do()
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				r.readChan <- Record{
					data: *hit.Source,
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	default:
		optFns := []elasticV5.ClientOptionFunc{
			elasticV5.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV5.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV5.NewClient(optFns...)
		if err != nil {
			return err
		}
		scroll := client.Scroll(r.esindex).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			ctx := context.Background()
			results, err := scroll.ScrollId(r.offset).Do(ctx)
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				r.readChan <- Record{
					data: *hit.Source,
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	}
}

func (r *Reader) execWithCron() error {
	// Create a client
	switch r.esVersion {
	case ElasticVersion6:
		optFns := []elasticV6.ClientOptionFunc{
			elasticV6.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV6.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV6.NewClient(optFns...)
		if err != nil {
			return err
		}
		var rangeQuery *elasticV6.RangeQuery
		if r.cronOffsetValueIsValid {
			rangeQuery = elasticV6.NewRangeQuery(r.cronOffsetKey).Gte(r.cronOffsetValue)
		} else {
			rangeQuery = elasticV6.NewRangeQuery(r.cronOffsetKey)
		}
		scroll := client.Scroll(r.esindex).Query(rangeQuery).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			ctx := context.Background()
			results, err := scroll.ScrollId(r.offset).Do(ctx)
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				m := make(map[string]interface{})
				jsoniter.Unmarshal(*hit.Source, &m)
				r.readChan <- Record{
					data:       *hit.Source,
					cronOffset: m[r.cronOffsetKey],
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	case ElasticVersion3:
		optFns := []elasticV3.ClientOptionFunc{
			elasticV3.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV3.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV3.NewClient(optFns...)
		if err != nil {
			return err
		}
		var rangeQuery *elasticV3.RangeQuery
		if r.cronOffsetValueIsValid {
			rangeQuery = elasticV3.NewRangeQuery(r.cronOffsetKey).Gte(r.cronOffsetValue)
		} else {
			rangeQuery = elasticV3.NewRangeQuery(r.cronOffsetKey)
		}
		scroll := client.Scroll(r.esindex).Query(rangeQuery).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			results, err := scroll.ScrollId(r.offset).Do()
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				m := make(map[string]interface{})
				jsoniter.Unmarshal(*hit.Source, &m)
				r.readChan <- Record{
					data:       *hit.Source,
					cronOffset: m[r.cronOffsetKey],
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	default:
		optFns := []elasticV5.ClientOptionFunc{
			elasticV5.SetURL(r.eshost),
		}

		if len(r.authUsername) > 0 && len(r.authPassword) > 0 {
			optFns = append(optFns, elasticV5.SetBasicAuth(r.authUsername, r.authPassword))
		}
		client, err := elasticV5.NewClient(optFns...)
		if err != nil {
			return err
		}
		var rangeQuery *elasticV5.RangeQuery
		if r.cronOffsetValueIsValid {
			rangeQuery = elasticV5.NewRangeQuery(r.cronOffsetKey).Gte(r.cronOffsetValue)
		} else {
			rangeQuery = elasticV5.NewRangeQuery(r.cronOffsetKey)
		}
		scroll := client.Scroll(r.esindex).Query(rangeQuery).Type(r.estype).Size(r.readBatch).KeepAlive(r.keepAlive)
		for {
			ctx := context.Background()
			results, err := scroll.ScrollId(r.offset).Do(ctx)
			if err == io.EOF {
				return nil // all results retrieved
			}
			if err != nil {
				return err // something went wrong
			}

			// Send the hits to the hits channel
			for _, hit := range results.Hits.Hits {
				m := make(map[string]interface{})
				jsoniter.Unmarshal(*hit.Source, &m)
				r.readChan <- Record{
					data:       *hit.Source,
					cronOffset: m[r.cronOffsetKey],
				}
			}
			r.offset = results.ScrollId
			if r.isStopping() || r.hasStopped() {
				return nil
			}
		}
	}
}

func (r *Reader) Start() error {
	if r.isStopping() || r.hasStopped() {
		return errors.New("reader is stopping or has stopped")
	} else if !atomic.CompareAndSwapInt32(&r.status, StatusInit, StatusRunning) {
		log.Warnf("Runner[%v] %q daemon has already started and is running", r.meta.RunnerName, r.Name())
		return nil
	}

	if r.isLoop {
		go func() {
			ticker := time.NewTicker(r.loopDuration)
			defer ticker.Stop()
			for {
				r.run()

				select {
				case <-r.stopChan:
					atomic.StoreInt32(&r.status, StatusStopped)
					log.Infof("Runner[%v] %q daemon has stopped from running", r.meta.RunnerName, r.Name())
					return
				case <-ticker.C:
				}
			}
		}()
	} else {
		if r.execOnStart {
			go r.run()
		}
		r.Cron.Start()
	}
	log.Infof("Runner[%v] %q daemon has started", r.meta.RunnerName, r.Name())
	return nil
}

func (r *Reader) Source() string {
	return r.eshost + "_" + r.esindex + "_" + r.estype
}

func (r *Reader) ReadLine() (string, error) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case rec := <-r.readChan:
		if !r.isLoop {
			r.cronOffsetValue = rec.cronOffset
			r.cronOffsetValueIsValid = true
		}
		return string(rec.data), nil
	case err := <-r.errChan:
		return "", err
	case <-timer.C:
	}

	return "", nil
}

func (r *Reader) Status() StatsInfo {
	r.statsLock.RLock()
	defer r.statsLock.RUnlock()
	return r.stats
}

func (r *Reader) Reset() error {
	if err := os.RemoveAll(r.metaFile); err != nil {
		return err
	}
	r.cronOffsetValueIsValid = false
	return nil
}

// SyncMeta 从队列取数据时同步队列，作用在于保证数据不重复
func (r *Reader) SyncMeta() {
	if err := r.meta.WriteOffset(r.offset, 0); err != nil {
		log.Errorf("Runner[%v] reader %q sync meta failed: %v", r.meta.RunnerName, r.Name(), err)
	}
	if !r.isLoop {
		err := ioutil.WriteFile(r.metaFile, []byte(fmt.Sprintf("%s", r.cronOffsetValue)), 0644)
		if err != nil {
			log.Errorf("Runner[%v] %v failed to sync meta: %v", r.meta.RunnerName, r.Name(), err)
		}
	}
	return
}

func (r *Reader) Close() error {
	if !atomic.CompareAndSwapInt32(&r.status, StatusRunning, StatusStopping) {
		log.Warnf("Runner[%v] reader %q is not running, close operation ignored", r.meta.RunnerName, r.Name())
		return nil
	}
	log.Debugf("Runner[%v] %q daemon is stopping", r.meta.RunnerName, r.Name())
	close(r.stopChan)

	// 如果此时没有 routine 正在运行，则在此处关闭数据管道，否则由 routine 在退出时负责关闭
	if atomic.CompareAndSwapInt32(&r.routineStatus, StatusInit, StatusStopping) {
		close(r.readChan)
		close(r.errChan)
	}
	return nil
}
