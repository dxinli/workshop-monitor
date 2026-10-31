package ingest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"workshop-monitor/internal/adapter/ingest"
)

// fakeClaimer 是 Claimer 的内存实现。单测不连数据库，DB 适配器另有实现。
type fakeClaimer struct {
	seen  map[string]bool
	calls []string
	err   error
}

func newFakeClaimer() *fakeClaimer { return &fakeClaimer{seen: map[string]bool{}} }

func (f *fakeClaimer) Claim(_ context.Context, terminalID string, seq int64, _ ingest.LogInfo) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	key := terminalID + "|" + itoa(seq)
	f.calls = append(f.calls, key)
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func okInfo() ingest.LogInfo {
	return ingest.LogInfo{
		MsgType:     "DATA",
		CollectTime: time.UnixMilli(1793500200123),
		ReceiveTime: time.UnixMilli(1793500200350),
		PointCount:  14,
		GoodCount:   14,
		ParseStatus: "OK",
	}
}

// 首次出现的 (terminalId, seq) 必须放行，且业务写入被执行一次。
func TestGuardFirstMessageIsAccepted(t *testing.T) {
	claim := newFakeClaimer()
	idem := ingest.NewIdem(claim)

	writes := 0
	status, err := idem.Guard(context.Background(), "ESN-ZX-0001", 10245, okInfo(), func(context.Context) error {
		writes++
		return nil
	})

	if err != nil {
		t.Fatalf("首次报文不应报错: %v", err)
	}
	if status != ingest.StatusAccepted {
		t.Fatalf("首次报文应判 OK，实际 %q", status)
	}
	if writes != 1 {
		t.Fatalf("首次报文应写业务表 1 次，实际 %d 次", writes)
	}
}

// 重复的 (terminalId, seq) 判 DUPLICATE，且不得写业务表。
// 这是「幂等在写任何业务表之前」的唯一落点：判重早于写入，写函数不会被调用。
func TestGuardDuplicateSkipsWrites(t *testing.T) {
	claim := newFakeClaimer()
	idem := ingest.NewIdem(claim)
	ctx := context.Background()

	writes := 0
	write := func(context.Context) error {
		writes++
		return nil
	}

	if _, err := idem.Guard(ctx, "ESN-ZX-0001", 10245, okInfo(), write); err != nil {
		t.Fatalf("首次报文不应报错: %v", err)
	}
	status, err := idem.Guard(ctx, "ESN-ZX-0001", 10245, okInfo(), write)

	if err != nil {
		t.Fatalf("重复报文不应报错: %v", err)
	}
	if status != ingest.StatusDuplicate {
		t.Fatalf("重复报文应判 DUPLICATE，实际 %q", status)
	}
	if writes != 1 {
		t.Fatalf("重复报文不得写业务表：两次 Guard 合计应只写 1 次，实际 %d 次", writes)
	}
}

// 同一序号在不同终端下是两条不同的消息，不能互相判重。
func TestGuardScopesSeqByTerminal(t *testing.T) {
	claim := newFakeClaimer()
	idem := ingest.NewIdem(claim)
	ctx := context.Background()
	noop := func(context.Context) error { return nil }

	if _, err := idem.Guard(ctx, "ESN-ZX-0001", 7, okInfo(), noop); err != nil {
		t.Fatalf("终端 1 首次报文不应报错: %v", err)
	}
	status, err := idem.Guard(ctx, "ESN-ZX-0002", 7, okInfo(), noop)
	if err != nil {
		t.Fatalf("终端 2 首次报文不应报错: %v", err)
	}
	if status != ingest.StatusAccepted {
		t.Fatalf("不同终端的同序号应各自放行，实际 %q", status)
	}
}

// 登记失败时不能继续写业务表，否则会出现「没记上幂等键却已入库」的脏数据。
func TestGuardClaimErrorStopsWrites(t *testing.T) {
	claim := newFakeClaimer()
	claim.err = errors.New("连接池耗尽")
	idem := ingest.NewIdem(claim)

	writes := 0
	status, err := idem.Guard(context.Background(), "ESN-ZX-0001", 10245, okInfo(), func(context.Context) error {
		writes++
		return nil
	})

	if err == nil {
		t.Fatal("登记失败时应返回错误")
	}
	if !errors.Is(err, claim.err) {
		t.Fatalf("错误应包装原始错误，实际 %v", err)
	}
	if status != "" {
		t.Fatalf("登记失败时不应给出结论，实际 %q", status)
	}
	if writes != 0 {
		t.Fatalf("登记失败时不得写业务表，实际写了 %d 次", writes)
	}
}
