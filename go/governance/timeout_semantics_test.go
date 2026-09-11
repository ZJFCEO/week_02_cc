package governance

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestTimeoutDoesNotCancelGoroutine 证明 Go 和 Python 在超时语义上的本质区别。
//
// Python 的 asyncio.timeout 会把协程真正取消在它当时停住的那个 await 上，
// 后面的代码一行都不会执行；Go 不能从外部中断 goroutine，
// context 只是一个"请你自己停下来"的信号。
//
// 下面这个 handler 故意不看 ctx，于是：
//   - Invoke 在超时后就返回了（等待被放弃）
//   - 但那个 goroutine 还在跑，并且在超时之后真的把副作用做了出来
//
// 这正是 TIMEOUT_UNKNOWN 存在的理由：框架无权断言副作用有没有发生。
// 工程上的应对是两条腿走路——handler 必须尊重 ctx（transferHandler 就是这么写的），
// 同时非幂等写操作对外只能承认"结果未知"，由调用方带幂等键去查证，而不是直接重试。
func TestTimeoutDoesNotCancelGoroutine(t *testing.T) {
	ctx := setup(t)

	var executed atomic.Int32
	stubborn := ToolDefinition{
		Name:        "stubborn",
		Description: "一个不看 ctx 的 handler",
		NewArgs:     func() Args { return &GetOrderArgs{} },
		Parameters:  objectSchema(map[string]any{"order_id": map[string]any{"type": "string"}}, "order_id"),
		Policy: ToolPolicy{
			Effect: EffectWrite, Risk: RiskLow, Permission: PermOrderRead,
			RequiresApproval: false, Timeout: 50 * time.Millisecond, MaxRetries: 0, Idempotent: false,
		},
		Handler: func(_ context.Context, _ string, _ Args, _ ExecutionContext) (map[string]any, error) {
			// 没有 select，没有 ctx.Done()，就是一路睡到底——然后照样产生副作用。
			time.Sleep(300 * time.Millisecond)
			executed.Add(1)
			return map[string]any{"done": true}, nil
		},
		CanonicalTarget: func(args Args) string { return args.(*GetOrderArgs).OrderID },
	}

	approvals := NewApprovalStore()
	sink := NewAuditSink()
	runtime := NewToolRuntime([]ToolDefinition{stubborn}, NewPermissionEngine(nil, approvals), sink)
	ec := BaseContext().WithAllowedTools(NewSet("stubborn"))

	started := time.Now()
	result := runtime.Invoke(ctx, ToolCall{"call_stubborn", "stubborn", RawArgs(`{"order_id":"ord_1001"}`)}, ec)
	elapsed := time.Since(started)

	if result.Code != "TIMEOUT_UNKNOWN" {
		t.Fatalf("期望 TIMEOUT_UNKNOWN，实际 %s", result.Code)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("Invoke 应该在超时后就返回，实际等了 %v", elapsed)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("返回的这一刻副作用还不该发生，实际 %d", got)
	}

	// 关键的一步：等 handler 自己跑完，看看副作用到底有没有发生。
	time.Sleep(400 * time.Millisecond)
	if got := executed.Load(); got != 1 {
		t.Fatalf("超时之后 goroutine 仍在继续，副作用应当已经发生，实际 %d", got)
	}

	t.Log("调用方收到的是 TIMEOUT_UNKNOWN，而副作用在超时之后真的发生了——这就是'未知'的含义")
}
