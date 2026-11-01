package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeqAllocatorRestartsWithoutReset 验「重启不归零」这条铁律：
// 分配器只能从持久化的高水位继续，不能从 1 重新开始。
func TestSeqAllocatorRestartsWithoutReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seq")

	first, err := LoadSeqAllocator(path)
	if err != nil {
		t.Fatalf("首次载入: %v", err)
	}
	for want := int64(1); want <= 3; want++ {
		got, err := first.Next()
		if err != nil {
			t.Fatalf("分配第 %d 个序号: %v", want, err)
		}
		if got != want {
			t.Fatalf("首次运行第 %d 次分配 = %d，期望 %d", want, got, want)
		}
	}

	// 模拟进程重启：同一路径重新载入。
	second, err := LoadSeqAllocator(path)
	if err != nil {
		t.Fatalf("重启载入: %v", err)
	}
	got, err := second.Next()
	if err != nil {
		t.Fatalf("重启后分配: %v", err)
	}
	if got != 4 {
		t.Fatalf("重启后首个序号 = %d，期望 4（不得归零）", got)
	}
}

// TestSeqAllocatorMissingFileStartsAtOne 验首次启动（没有序号文件）从 1 开始。
func TestSeqAllocatorMissingFileStartsAtOne(t *testing.T) {
	a, err := LoadSeqAllocator(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("载入: %v", err)
	}
	got, err := a.Next()
	if err != nil {
		t.Fatalf("分配: %v", err)
	}
	if got != 1 {
		t.Fatalf("首个序号 = %d，期望 1", got)
	}
}

// TestSeqAllocatorRejectsGarbage 序号文件被写坏时必须报错，
// 静默从头开始会重发已用过的序号，那些报文会被接入段判成重复后丢掉。
func TestSeqAllocatorRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seq")
	if err := writeFile(path, "not-a-number\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeqAllocator(path); err == nil {
		t.Fatal("序号文件内容非法时应报错")
	}
}

// TestSeqAllocatorCreatesMissingDir 验序号文件的父目录不存在时先建出来：
// 默认路径 var/seq.state 在首次部署时还没有 var/，这里报错等于拒绝启动。
func TestSeqAllocatorCreatesMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "seq.state")
	a, err := LoadSeqAllocator(path)
	if err != nil {
		t.Fatalf("父目录不存在时应建出目录：%v", err)
	}
	if seq, err := a.Next(); err != nil || seq != 1 {
		t.Fatalf("首次分配 = %d, err=%v，期望 1", seq, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("序号文件未落盘：%v", err)
	}
}

// writeFile 是测试用的落盘助手，放在包内避免测试文件互相 import。
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
