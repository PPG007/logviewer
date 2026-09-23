package service

import (
	"os"
	"path/filepath"
	"testing"

	"logviewer/internal/filecache"
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
	// 缓存目录同样指向临时目录：否则服务层测试会把远端内容的缓存写进真实的
	// %LocalAppData%\logviewer\cache，既污染用户环境又让用例互相干扰。
	os.Setenv(filecache.EnvCacheDir, filepath.Join(dir, "cache"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
