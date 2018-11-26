package script

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/reader"
	. "github.com/qiniu/logkit/reader/config"
	. "github.com/qiniu/logkit/reader/test"
)

func Test_scriptFile(t *testing.T) {
	fileName := filepath.Join(os.TempDir(), "scriptFile.sh")

	//create file & write file
	CreateFile(fileName, "echo \"hello world\"")
	defer DeleteFile(fileName)

	readerConf := conf.MapConf{
		KeyExecInterpreter: "bash",
		KeyLogPath:         fileName,
	}
	meta, err := reader.NewMetaWithConf(readerConf)
	if err != nil {
		t.Error(err)
	}
	defer os.RemoveAll("./meta")

	r, err := NewReader(meta, readerConf)
	if err != nil {
		t.Error(err)
	}
	assert.NoError(t, err)
	sr := r.(*Reader)
	assert.NoError(t, sr.Start())
	defer sr.Close()

	data, err := r.ReadLine()
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, "hello world\n", data)
}

func Test_checkPath(t *testing.T) {
	fileName := filepath.Join(os.TempDir(), "scriptFile.sh")
	go func() {
		time.Sleep(2 * time.Second)
		//create file & write file
		CreateFile(fileName, "echo \"hello world\"")
	}()
	defer DeleteFile(fileName)

	waitTime = 4 * time.Second
	readerConf := conf.MapConf{
		KeyExecInterpreter: "bash",
		KeyLogPath:         fileName,
	}
	meta, err := reader.NewMetaWithConf(readerConf)
	if err != nil {
		t.Error(err)
	}
	res, err := checkPath(meta, fileName)
	assert.NoError(t, err)
	assert.Equal(t, fileName, res)
}
