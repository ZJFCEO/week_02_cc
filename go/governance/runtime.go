package governance

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// 七、执行入口（整个框架的主干）
// ---------------------------------------------------------------------------

// ErrToolTimeout 表示一次执行等超时了。
//
// 注意措辞是"等超时了"而不是"执行被取消了"，原因见 executeWithRecovery 里的说明。
var ErrToolTimeout = errors.New("tool execution timeout")

// ToolRuntime 是模型、CLI、测试共用的唯一工具执行入口。
// 一旦有人绕过它直接调 handler，上面所有治理层就全部形同虚设。
type ToolRuntime struct {
	tools     map[string]ToolDefinition
	engine    *PermissionEngine
	auditSink *AuditSink
}

// NewToolRuntime 组装运行时。
func NewToolRuntime(tools []ToolDefinition, engine *PermissionEngine, sink *AuditSink) *ToolRuntime {
	indexed := make(map[string]ToolDefinition, len(tools))
	for _, tool := range tools {
		indexed[tool.Name] = tool
	}
	return &ToolRuntime{tools: indexed, engine: engine, auditSink: sink}
}

// ModelTools 是发现期：告诉模型"你这轮有哪些工具可用"。
//
// 两层收窄：先按白名单过滤工具，再用 ToModelTool 把 Handler 和 Policy 摘掉。
// 注意这只是减少误用，不是安全边界——真正的边界在 Invoke 里（见 Decide 第 3 步）。
func (r *ToolRuntime) ModelTools(ec ExecutionContext) []map[string]any {
	out := make([]map[string]any, 0, len(r.tools))
	for _, tool := range r.tools {
		if ec.AllowedTools.Has(tool.Name) {
			out = append(out, tool.ToModelTool())
		}
	}
	return out
}

// Invoke 是一次工具调用的完整生命周期，四个阶段依次推进，顺序不可交换：
//
//	prepare-1  参数校验：把不可信 JSON 变成业务对象
//	prepare-2  执行期授权：三态决策 + 写决策审计
//	execute    真正执行：超时、重试、错误都在这里收敛
//	finalize   收尾：脱敏 -> 写执行审计 -> 返回模型可见结果
//
// 重要约定：这个方法永远不返回 error，所有失败都变成一个 ToolResult。
// 因为调用方是模型，它需要的是一个能读懂的错误码，而不是一段调用栈。
func (r *ToolRuntime) Invoke(ctx context.Context, call ToolCall, ec ExecutionContext) ToolResult {
	started := time.Now()

	tool, ok := r.tools[call.Name]
	if !ok {
		// 模型报了一个根本不存在的工具名——这在真实链路里很常见，直接拒绝并留痕。
		return r.rejected(call, ec, "TOOL_NOT_FOUND", "工具不存在")
	}

	// ---------- 阶段一：参数校验 ----------
	// DecodeArgs 相当于 json.Unmarshal + 全部校验规则一次跑完。
	// 过了这一关，后面所有代码拿到的都是合法对象，不必再写一遍防御性判断。
	args := tool.NewArgs()
	if err := DecodeArgs(call.Arguments, args); err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			// 只给结构化信息，让模型知道该改哪个字段、怎么改。
			return r.rejected(call, ec, "INVALID_ARGUMENT", invalid.Fields)
		}
		return r.rejected(call, ec, "INVALID_ARGUMENT", err.Error())
	}

	// ---------- 阶段二：执行期授权 ----------
	// 无论决策是什么，都先写一条 decision 审计——被拒绝的调用同样要留痕。
	decision := r.engine.Decide(ctx, tool, args, ec)
	r.auditSink.Append(AuditRecord{
		TraceID:      ec.TraceID,
		ToolCallID:   call.ToolCallID,
		ToolName:     call.Name,
		UserID:       ec.UserID,
		TenantID:     ec.TenantID,
		Phase:        "decision",
		Decision:     string(decision.Action),
		Code:         decision.Code,
		ArgumentKeys: ArgumentKeys(call.Arguments), // 只记参数名并排序，不记参数值
	})

	if decision.Action != ActionAllow {
		// DENY 和 CONFIRM 都在这里返回。注意到此为止，handler 一次都没有被碰过。
		return ToolResult{
			ToolCallID: call.ToolCallID,
			ToolName:   call.Name,
			OK:         false,
			Action:     decision.Action,
			Code:       decision.Code,
			Content:    decision.Reason,
		}
	}

	// ---------- 阶段三：执行 ----------
	raw, err := r.executeWithRecovery(ctx, tool, call.ToolCallID, args, ec)
	if err != nil {
		var denied *PolicyError
		switch {
		case errors.Is(err, ErrToolTimeout):
			// 超时的语义由幂等性决定，这是这一段最值得琢磨的几行：
			//   只读或幂等   -> TIMEOUT，调用方可以安全重试
			//   非幂等写操作 -> TIMEOUT_UNKNOWN，框架无权断言副作用有没有发生
			code := "TIMEOUT_UNKNOWN"
			if tool.Policy.Effect == EffectRead || tool.Policy.Idempotent {
				code = "TIMEOUT"
			}
			return r.failed(call, ec, started, code, "工具执行超时")
		case errors.As(err, &denied):
			// handler 内部返回的业务拒绝，例如转入账户不存在（ACCOUNT_NOT_FOUND）。
			return r.failed(call, ec, started, denied.Code, denied.Message)
		default:
			// 生产中应映射异常类型，不要把内部错误原文交给模型。
			return r.failed(call, ec, started, "TOOL_ERROR", err.Error())
		}
	}

	// ---------- 阶段四：脱敏与审计 ----------
	// handler 返回的是明文数据（账号、邮箱、token 都在里面），
	// 在这里统一过一遍 Redact 之后，才允许进入模型上下文。
	safeContent := Redact(toAnyMap(raw))
	latency := time.Since(started).Milliseconds()
	r.auditSink.Append(AuditRecord{
		TraceID:      ec.TraceID,
		ToolCallID:   call.ToolCallID,
		ToolName:     call.Name,
		UserID:       ec.UserID,
		TenantID:     ec.TenantID,
		Phase:        "execution",
		Decision:     "executed",
		Code:         "OK",
		ArgumentKeys: ArgumentKeys(call.Arguments),
		LatencyMS:    latency,
	})

	return ToolResult{
		ToolCallID: call.ToolCallID,
		ToolName:   call.Name,
		OK:         true,
		Action:     ActionAllow,
		Code:       "OK",
		Content:    safeContent,
	}
}

// executeWithRecovery 是带重试和超时的执行包装。两条策略：
//
//  1. 只有只读或幂等的工具才允许重试，非幂等写操作一律不重试（转账属于这一类）
//  2. 每次尝试都套一层超时
//
// 【Go 差异·重要】这里和 Python 版有本质区别，也是这份复刻最值得看的一段：
//
// Python 的 asyncio.timeout 能真的把协程取消在它当时停住的那个 await 上，
// 后面的代码一行都不会执行。Go 没有这种能力——goroutine 不能被外部中断。
// context 只是一个"请你自己停下来"的信号，handler 不主动检查 ctx.Done() 就会一直跑到底。
//
// 所以下面这段 select 的真实语义是"放弃等待"，而不是"取消执行"：
// 超时返回后，那个 goroutine 可能还在跑，甚至可能在超时之后才把钱扣掉。
// 这恰恰说明了 TIMEOUT_UNKNOWN 为什么必须存在——在 Go 里它比在 Python 里更名副其实。
//
// 工程上的应对是两条腿走路：handler 必须尊重 ctx（本文件的 transferHandler 就是这么写的），
// 同时非幂等写操作对外只能承认"结果未知"，由调用方带幂等键去查证，而不是直接重试。
func (r *ToolRuntime) executeWithRecovery(ctx context.Context, tool ToolDefinition, toolCallID string, args Args, ec ExecutionContext) (map[string]any, error) {
	retries := 0
	if tool.Policy.Effect == EffectRead || tool.Policy.Idempotent {
		retries = tool.Policy.MaxRetries
	}

	type handlerOutcome struct {
		out map[string]any
		err error
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, tool.Policy.Timeout)

		// 缓冲容量 1：即使这边已经不等了，handler 结束时也能把结果丢进来不阻塞，
		// 否则那个 goroutine 会永远卡在发送上，成为真正的泄漏。
		done := make(chan handlerOutcome, 1)
		go func() {
			out, err := tool.Handler(callCtx, toolCallID, args, ec)
			done <- handlerOutcome{out: out, err: err}
		}()

		select {
		case <-callCtx.Done():
			cancel()
			return nil, ErrToolTimeout
		case outcome := <-done:
			cancel()
			if outcome.err == nil {
				return outcome.out, nil
			}
			var transient *TransientError
			if errors.As(outcome.err, &transient) {
				lastErr = outcome.err
				if attempt == retries {
					return nil, outcome.err
				}
				// 指数退避，最多等 200 毫秒。
				backoff := time.Duration(50<<attempt) * time.Millisecond
				if backoff > 200*time.Millisecond {
					backoff = 200 * time.Millisecond
				}
				time.Sleep(backoff)
				continue
			}
			return nil, outcome.err
		}
	}
	return nil, lastErr
}

// rejected 是决策阶段的拒绝出口：写一条 decision 审计，再返回统一结构的 ToolResult。
func (r *ToolRuntime) rejected(call ToolCall, ec ExecutionContext, code string, content any) ToolResult {
	r.auditSink.Append(AuditRecord{
		TraceID:      ec.TraceID,
		ToolCallID:   call.ToolCallID,
		ToolName:     call.Name,
		UserID:       ec.UserID,
		TenantID:     ec.TenantID,
		Phase:        "decision",
		Decision:     "deny",
		Code:         code,
		ArgumentKeys: ArgumentKeys(call.Arguments),
	})
	return ToolResult{
		ToolCallID: call.ToolCallID,
		ToolName:   call.Name,
		OK:         false,
		Action:     ActionDeny,
		Code:       code,
		Content:    content,
	}
}

// failed 是执行阶段的失败出口：写一条 execution 审计（带耗时），再返回统一结构的 ToolResult。
// 与 rejected 的区别在于 Phase 不同——一个是没让做，一个是做了但没成。
func (r *ToolRuntime) failed(call ToolCall, ec ExecutionContext, started time.Time, code string, content any) ToolResult {
	r.auditSink.Append(AuditRecord{
		TraceID:      ec.TraceID,
		ToolCallID:   call.ToolCallID,
		ToolName:     call.Name,
		UserID:       ec.UserID,
		TenantID:     ec.TenantID,
		Phase:        "execution",
		Decision:     "failed",
		Code:         code,
		ArgumentKeys: ArgumentKeys(call.Arguments),
		LatencyMS:    time.Since(started).Milliseconds(),
	})
	return ToolResult{
		ToolCallID: call.ToolCallID,
		ToolName:   call.Name,
		OK:         false,
		Action:     ActionDeny,
		Code:       code,
		Content:    content,
	}
}

// toAnyMap 把 handler 返回的 map 转成 Redact 认识的形状。
func toAnyMap(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = value
	}
	return out
}

// MarshalResult 把结果序列化成一行 JSON，演示输出用。
func MarshalResult(result ToolResult) string {
	raw, err := json.Marshal(result)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
