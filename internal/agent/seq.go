package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SeqAllocator 在单个终端内单调递增地分配报文序号（接口规范 §9.1）。
//
// 重启不归零：启动时先读回持久化的高水位，再从它继续分配。数据、事件、状态
// 共用同一个序号空间，同一次采集的多个测点共享一个序号（BR-04）。
type SeqAllocator struct {
	mu    sync.Mutex
	saved int64
	path  string
}

// LoadSeqAllocator 读回上次的高水位。文件不存在视为首次启动（从 1 起分配）。
//
// path 为空表示不做持久化（单次运行、无重启续接需求时使用）。
// 所在目录不存在时先建出来：默认路径 var/seq.state 在首次部署时还没有 var/。
func LoadSeqAllocator(path string) (*SeqAllocator, error) {
	a := &SeqAllocator{path: path}
	if path == "" {
		return a, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("建序号文件目录 %s: %w", filepath.Dir(path), err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, fmt.Errorf("读取序号文件 %s: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return a, nil
	}
	saved, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("序号文件 %s 内容不是整数（%q）: %w", path, text, err)
	}
	if saved < 0 {
		return nil, fmt.Errorf("序号文件 %s 内容为负数（%d）", path, saved)
	}
	a.saved = saved
	return a, nil
}

// Next 分配下一个序号。
//
// 先落盘高水位再返回：进程在「分配了但没发出去」之间崩溃时宁可跳号，
// 不能重号——重号会被接入段的消息级幂等判成重复，整条数据静默丢失。
func (a *SeqAllocator) Next() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	next := a.saved + 1
	if err := a.persist(next); err != nil {
		return 0, err
	}
	a.saved = next
	return next, nil
}

// Current 返回已分配到的最后一个序号，供退出路径与日志取证使用。
func (a *SeqAllocator) Current() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saved
}

// persist 原子写入高水位：同目录临时文件 + rename，避免半个数字被读回。
func (a *SeqAllocator) persist(seq int64) error {
	if a.path == "" {
		return nil
	}
	dir := filepath.Dir(a.path)
	tmp, err := os.CreateTemp(dir, ".seq-*")
	if err != nil {
		return fmt.Errorf("写序号文件 %s: %w", a.path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后文件已不在，失败时兜底清理

	if _, err := fmt.Fprintf(tmp, "%d\n", seq); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写序号文件 %s: %w", a.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写序号文件 %s: %w", a.path, err)
	}
	if err := os.Rename(tmpName, a.path); err != nil {
		return fmt.Errorf("写序号文件 %s: %w", a.path, err)
	}
	return nil
}
