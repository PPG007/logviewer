package service

import (
	"os"
	"path/filepath"
	"testing"

	"logviewer/internal/store"
)

// TestMain 把历史记录数据库指向临时文件：单测不得读写用户真实的
// %AppData%/logviewer/logviewer.db（TestMain 在内部/外部测试包间共享，两处测试都受其约束）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "logviewer-test-db-")
	if err != nil {
		panic(err)
	}
	os.Setenv(store.EnvDBPath, filepath.Join(dir, "test.db"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
