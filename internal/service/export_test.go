package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exportHits 白盒测试：相邻命中合并批量读、跨间隔逐段读，内容与原文一致且行尾统一 \n。
func TestExportHits(t *testing.T) {
	lines := []string{
		`{"n":0}`,
		`not json line 1`,
		`{"n":2}`,
		`{"n":3}`,
		`{"n":4}`,
		`{"n":5}`,
	}
	svc := NewLogService()
	info, err := svc.OpenFile(writeTempLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(info.ID)
	waitReadySvc(t, svc, info.ID)
	sess := svc.sessions[info.ID]

	var buf bytes.Buffer
	// 命中 [1,3,4]：1 单行、3-4 连续两行
	if err := exportHits(&buf, sess, []int64{1, 3, 4}); err != nil {
		t.Fatal(err)
	}
	want := lines[1] + "\n" + lines[3] + "\n" + lines[4] + "\n"
	if buf.String() != want {
		t.Fatalf("导出内容 = %q, want %q", buf.String(), want)
	}

	// 空命中：无输出
	buf.Reset()
	if err := exportHits(&buf, sess, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatal("空命中应无输出")
	}

	// 乱序输入（引擎保证升序，防御性验证：不 panic 即可）
	if err := exportHits(&buf, sess, []int64{4, 3}); err != nil {
		t.Fatal(err)
	}
}

func TestExportMatchesNoApp(t *testing.T) {
	// 无 Wails app 环境（单测）：ExportMatches 直接报错，不走对话框
	svc := NewLogService()
	if _, err := svc.ExportMatches("ghost", "tab"); err == nil {
		t.Fatal("无 app 环境 ExportMatches 应报错")
	}
}

// ——— 本文件白盒测试辅助 ———

func writeTempLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "export.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitReadySvc(t *testing.T, svc *LogService, fileID string) {
	t.Helper()
	for range 1000 {
		st, err := svc.GetIndexStatus(fileID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Done {
			if st.Error != "" {
				t.Fatalf("索引失败: %s", st.Error)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待索引完成超时")
}
