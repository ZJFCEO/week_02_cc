// 第二章作业新增的 5 个转账测试，与 Python 版 tests/test_tool_governance.py 末尾 5 个一一对应。
//
// 所有调用都经过 ToolRuntime.Invoke，不直接调 transferHandler。
// 标了【Go 差异】的断言是 Python 版没有、Go 版额外加的。
package governance

import (
	"fmt"
	"maps"
	"reflect"
	"testing"
	"time"
)

// 对应 test_transfer_schema_rejects_invalid_arguments
// 任务 2：格式、范围、额外字段都挡在参数校验这一层，handler 碰都碰不到。
func TestTransferSchemaRejectsInvalidArguments(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)
	before := SnapshotAccounts()

	invalidArguments := []map[string]any{
		transferArguments(map[string]any{"from_account": "ACC-a-123456"}), // 小写字母，正则不匹配
		transferArguments(map[string]any{"to_account": "ACC-A-12345"}),    // 只有 5 位数字
		transferArguments(map[string]any{"amount": 0.0}),                  // 金额必须大于 0
		transferArguments(map[string]any{"amount": 100_000.01}),           // 超过 Schema 上限
		transferArguments(map[string]any{"approved": true}),               // 注入越权字段，DisallowUnknownFields 拦截
		// 【Go 差异】金额写成字符串。Python 靠 strict=True 拒绝，Go 的 json 解码天生不做隐式转换。
		transferArguments(map[string]any{"amount": "1000"}),
	}
	for index, arguments := range invalidArguments {
		result := runtime.Invoke(ctx, ToolCall{fmt.Sprintf("transfer_schema_%d", index), "transfer", MustArgs(arguments)}, BaseContext())
		if result.Action != ActionDeny || result.Code != "INVALID_ARGUMENT" {
			t.Errorf("第 %d 组参数 %v：期望 deny/INVALID_ARGUMENT，实际 %s/%s", index, arguments, result.Action, result.Code)
		}
	}

	if !maps.Equal(SnapshotAccounts(), before) {
		t.Errorf("参数非法时账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("期望转账执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_transfer_precheck_denies_over_limit_and_insufficient_balance
// 任务 3：业务预检只判断不改账本，拒绝必须停在 decision 阶段。
func TestTransferPrecheckDeniesOverLimitAndInsufficientBalance(t *testing.T) {
	ctx := setup(t)
	runtime, _, audit := BuildRuntime(nil)
	before := SnapshotAccounts()

	overLimit := runtime.Invoke(ctx,
		ToolCall{"transfer_limit_01", "transfer", MustArgs(transferArguments(map[string]any{"amount": 60_000.0}))},
		BaseContext(),
	)
	if overLimit.Action != ActionDeny || overLimit.Code != "EXCEED_LIMIT" {
		t.Errorf("期望 deny/EXCEED_LIMIT，实际 %s/%s", overLimit.Action, overLimit.Code)
	}

	// ACC-A-888888 余额 20000，转 20000.01 差一分钱
	insufficient := runtime.Invoke(ctx,
		ToolCall{"transfer_balance_01", "transfer", MustArgs(transferArguments(map[string]any{
			"from_account": "ACC-A-888888",
			"amount":       20_000.01,
		}))},
		BaseContext(),
	)
	if insufficient.Action != ActionDeny || insufficient.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("期望 deny/INSUFFICIENT_BALANCE，实际 %s/%s", insufficient.Action, insufficient.Code)
	}

	if !maps.Equal(SnapshotAccounts(), before) {
		t.Errorf("预检拒绝时账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("期望转账执行 0 次，实际 %d 次", got)
	}
	// 审计里只有 decision 阶段：证明 handler 根本没被调用，而不是调用后回滚
	for _, record := range audit.Records() {
		if record.Phase != "decision" {
			t.Errorf("期望只有 decision 阶段的审计，实际出现 %s/%s", record.Phase, record.Code)
		}
	}
}

// 对应 test_transfer_requires_approval_then_executes_with_redacted_accounts
// 任务 5 + 任务 6：高风险写操作先 CONFIRM，审批后执行，返回给模型的账号已脱敏。
func TestTransferRequiresApprovalThenExecutesWithRedactedAccounts(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, audit := BuildRuntime(nil)
	ec := BaseContext()
	arguments := transferArguments(map[string]any{"amount": 1_200.0})
	fromBefore, _ := Balance("tenant_a", "ACC-A-123456")
	toBefore, _ := Balance("tenant_a", "ACC-A-654321")

	pending := runtime.Invoke(ctx, ToolCall{"transfer_pending", "transfer", MustArgs(arguments)}, ec)
	if pending.Action != ActionConfirm || pending.Code != "APPROVAL_REQUIRED" {
		t.Fatalf("期望 confirm/APPROVAL_REQUIRED，实际 %s/%s", pending.Action, pending.Code)
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("没有审批时不应执行，实际执行了 %d 次", got)
	}

	approvals.Approve("approval_transfer", ec, "transfer", arguments, 0)
	approved := runtime.Invoke(ctx,
		ToolCall{"transfer_approved", "transfer", MustArgs(arguments)},
		ec.WithApprovalID("approval_transfer"),
	)

	if !approved.OK {
		t.Fatalf("期望执行成功，实际 %s: %v", approved.Code, approved.Content)
	}
	want := map[string]any{
		"txn_id": "txn_proved", // tool_call_id 后 6 位
		"from":   "ACC-A-****3456",
		"to":     "ACC-A-****4321",
		"amount": 1_200.0,
		"status": "succeeded",
	}
	if !reflect.DeepEqual(approved.Content, want) {
		t.Errorf("返回内容不对\n期望 %v\n实际 %v", want, approved.Content)
	}

	fromAfter, _ := Balance("tenant_a", "ACC-A-123456")
	toAfter, _ := Balance("tenant_a", "ACC-A-654321")
	if fromAfter != fromBefore-1_200.0 || toAfter != toBefore+1_200.0 {
		t.Errorf("余额变动不对：%v -> %v，%v -> %v", fromBefore, fromAfter, toBefore, toAfter)
	}
	if got := SideEffect("transfer_executions"); got != 1 {
		t.Errorf("期望转账执行 1 次，实际 %d 次", got)
	}

	// 审计还原出"挂起 -> 审批放行 -> 执行成功"这条时间线
	type phaseCode struct{ Phase, Code string }
	var timeline []phaseCode
	records := audit.Records()
	for _, record := range records {
		timeline = append(timeline, phaseCode{record.Phase, record.Code})
	}
	wantTimeline := []phaseCode{
		{"decision", "APPROVAL_REQUIRED"},
		{"decision", "APPROVED"},
		{"execution", "OK"},
	}
	if !reflect.DeepEqual(timeline, wantTimeline) {
		t.Errorf("审计时间线不对\n期望 %v\n实际 %v", wantTimeline, timeline)
	}
	if last := records[len(records)-1]; last.ToolCallID != "transfer_approved" {
		t.Errorf("审计最后一条的 tool_call_id 期望 transfer_approved，实际 %s", last.ToolCallID)
	}
}

// 对应 test_transfer_approval_is_bound_to_canonical_arguments
// 审批绑定具体参数和具体的人：改金额、换用户都作废，并且只能用一次。
func TestTransferApprovalIsBoundToCanonicalArguments(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, _ := BuildRuntime(nil)
	ec := BaseContext()
	approvals.Approve("approval_bound", ec, "transfer", transferArguments(nil), 0)
	approvedCtx := ec.WithApprovalID("approval_bound")

	// 拿着为 1000 元签发的审批去转 2000 元
	tampered := runtime.Invoke(ctx,
		ToolCall{"transfer_tampered", "transfer", MustArgs(transferArguments(map[string]any{"amount": 2_000.0}))},
		approvedCtx,
	)
	if tampered.Action != ActionConfirm || tampered.Code != "APPROVAL_REQUIRED" {
		t.Errorf("改了金额应退回 confirm/APPROVAL_REQUIRED，实际 %s/%s", tampered.Action, tampered.Code)
	}

	// 同一份参数，换个人来用这张审批
	otherUser := runtime.Invoke(ctx,
		ToolCall{"transfer_other_user", "transfer", MustArgs(transferArguments(nil))},
		approvedCtx.WithUserID("u_999"),
	)
	if otherUser.Action != ActionConfirm {
		t.Errorf("换了用户应退回 confirm，实际 %s", otherUser.Action)
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("审批不匹配时不应执行，实际 %d 次", got)
	}

	first := runtime.Invoke(ctx, ToolCall{"transfer_first", "transfer", MustArgs(transferArguments(nil))}, approvedCtx)
	second := runtime.Invoke(ctx, ToolCall{"transfer_replay", "transfer", MustArgs(transferArguments(nil))}, approvedCtx)

	if !first.OK {
		t.Errorf("参数一致时应当成功，实际 %s", first.Code)
	}
	if second.Action != ActionConfirm { // 一次性凭证已核销
		t.Errorf("重放应退回 confirm，实际 %s", second.Action)
	}
	if got := SideEffect("transfer_executions"); got != 1 {
		t.Errorf("期望转账执行 1 次，实际 %d 次", got)
	}
}

// 对应 test_transfer_timeout_reports_unknown_without_side_effects
// 任务 4：非幂等写操作超时返回 TIMEOUT_UNKNOWN，且不能留下半成品副作用。
func TestTransferTimeoutReportsUnknownWithoutSideEffects(t *testing.T) {
	ctx := setup(t)
	// 限额 50000 会在预检阶段先挡住 80000 以上的转账，handler 的超时分支永远走不到；
	// 临时放开限额才能验证超时链路。对应 pytest 的 monkeypatch.setattr，用 t.Cleanup 还原。
	originalLimit := SingleTransferLimit
	SingleTransferLimit = 100_000.0
	t.Cleanup(func() { SingleTransferLimit = originalLimit })

	runtime, approvals, audit := BuildRuntime(nil)
	ec := BaseContext()
	arguments := transferArguments(map[string]any{"amount": 90_000.0})
	approvals.Approve("approval_timeout", ec, "transfer", arguments, 0)
	before := SnapshotAccounts()

	started := time.Now()
	result := runtime.Invoke(ctx,
		ToolCall{"transfer_timeout", "transfer", MustArgs(arguments)},
		ec.WithApprovalID("approval_timeout"),
	)
	elapsed := time.Since(started)

	if result.OK {
		t.Fatalf("期望超时失败，实际成功了")
	}
	if result.Code != "TIMEOUT_UNKNOWN" {
		t.Errorf("期望 TIMEOUT_UNKNOWN，实际 %s", result.Code)
	}
	// handler 里的等待排在改余额之前，并且尊重了 ctx，超时后账本一分未动
	if !maps.Equal(SnapshotAccounts(), before) {
		t.Errorf("超时后账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("超时后不应留下副作用，实际执行了 %d 次", got)
	}
	records := audit.Records()
	if last := records[len(records)-1]; last.Phase != "execution" || last.Code != "TIMEOUT_UNKNOWN" {
		t.Errorf("审计最后一条期望 execution/TIMEOUT_UNKNOWN，实际 %s/%s", last.Phase, last.Code)
	}
	// 【Go 差异】额外断言等待时长：超时保护 1.5 秒，handler 模拟 3 秒，应当贴近前者。
	// 这能证明 select 真的在超时那一刻放弃了等待，而不是傻等 handler 跑完。
	if elapsed > 2*time.Second {
		t.Errorf("超时保护没生效，等了 %v", elapsed)
	}
}
