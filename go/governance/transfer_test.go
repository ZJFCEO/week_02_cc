// 第二章作业：转账工具的治理链路测试（Go 版）。
//
// 所有调用都必须经过 ToolRuntime.Invoke，不允许直接调 transferHandler。
//
// go test 速查（对照 Python 的 pytest）：
//
//	Go                              pytest 里对应什么
//	-----------------------------   ----------------------------------------
//	func TestXxx(t *testing.T)      def test_xxx()，同样靠名字前缀发现
//	t.Fatalf / t.Errorf             assert，前者立即结束用例，后者继续往下跑
//	t.Cleanup(fn)                   fixture 里 yield 之后的 teardown
//	临时改包级变量 + t.Cleanup 还原   monkeypatch.setattr
//	go test -run Transfer           pytest -k transfer
//
// 注意这些用例共享包级账本，所以都不能调用 t.Parallel()。
package governance

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"
)

const (
	fromAccount  = "ACC-A-123456"
	toAccount    = "ACC-A-654321"
	smallAccount = "ACC-A-888888"
)

// setup 复位账本与副作用计数器，并在用例结束后再复位一次，保证用例之间互不污染。
func setup(t *testing.T) context.Context {
	t.Helper()
	ResetState()
	t.Cleanup(ResetState)
	return context.Background()
}

// transferArgs 构造一份合法的转账参数。
// 返回的是 map（模型发过来的样子），校验是 runtime 的职责——
// 测试要模拟的正是"未经校验的输入"。
func transferArgs(overrides map[string]any) map[string]any {
	args := map[string]any{
		"from_account": fromAccount,
		"to_account":   toAccount,
		"amount":       1000.0,
	}
	maps.Copy(args, overrides)
	return args
}

func contentMap(t *testing.T, result ToolResult) map[string]any {
	t.Helper()
	typed, ok := result.Content.(map[string]any)
	if !ok {
		t.Fatalf("期望 content 是 map，实际是 %T: %v", result.Content, result.Content)
	}
	return typed
}

// TestTransferRejectsInvalidArguments 对应【任务 2】：
// Schema 是第一道闸门，格式、范围与额外字段都进不了 handler。
func TestTransferRejectsInvalidArguments(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)
	ec := BaseContext()
	before := SnapshotAccounts()

	cases := map[string]map[string]any{
		"bad_format":      transferArgs(map[string]any{"from_account": "ACC-a-123456"}),
		"short_digits":    transferArgs(map[string]any{"to_account": "ACC-A-12345"}),
		"non_positive":    transferArgs(map[string]any{"amount": 0.0}),
		"over_schema_max": transferArgs(map[string]any{"amount": 100_000.01}),
		"extra_field":     transferArgs(map[string]any{"approved": true}),
		"wrong_type":      transferArgs(map[string]any{"amount": "1000"}),
	}

	for name, args := range cases {
		result := runtime.Invoke(ctx, ToolCall{"call_" + name, "transfer", MustArgs(args)}, ec)
		if result.OK {
			t.Errorf("%s: 期望被拒绝，实际放行了", name)
		}
		if result.Action != ActionDeny {
			t.Errorf("%s: 期望 deny，实际 %s", name, result.Action)
		}
		if result.Code != "INVALID_ARGUMENT" {
			t.Errorf("%s: 期望 INVALID_ARGUMENT，实际 %s", name, result.Code)
		}
	}

	// DisallowUnknownFields 必须明确报出多余字段，而不是被静默忽略。
	extra := runtime.Invoke(ctx, ToolCall{"call_extra", "transfer",
		MustArgs(transferArgs(map[string]any{"approved": true}))}, ec)
	fields, ok := extra.Content.([]FieldError)
	if !ok {
		t.Fatalf("期望 content 是 []FieldError，实际是 %T", extra.Content)
	}
	found := false
	for _, field := range fields {
		if field.Path == "approved" {
			found = true
		}
	}
	if !found {
		t.Errorf("期望报出 approved 字段，实际 %+v", fields)
	}

	if !maps.Equal(before, SnapshotAccounts()) {
		t.Errorf("参数非法时账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("期望 handler 一次都没执行，实际执行了 %d 次", got)
	}
}

// TestTransferPrecheckBlocksLimitAndInsufficientBalance 对应【任务 3】：
// 业务预检在权限之后、副作用之前，只判断不改账本。
func TestTransferPrecheckBlocksLimitAndInsufficientBalance(t *testing.T) {
	ctx := setup(t)
	runtime, _, audit := BuildRuntime(nil)
	ec := BaseContext()
	before := SnapshotAccounts()

	overLimit := runtime.Invoke(ctx, ToolCall{"call_limit", "transfer",
		MustArgs(transferArgs(map[string]any{"amount": 60_000.0}))}, ec)
	if overLimit.OK || overLimit.Action != ActionDeny || overLimit.Code != "EXCEED_LIMIT" {
		t.Errorf("期望 deny/EXCEED_LIMIT，实际 %s/%s", overLimit.Action, overLimit.Code)
	}

	poor := runtime.Invoke(ctx, ToolCall{"call_balance", "transfer",
		MustArgs(transferArgs(map[string]any{"from_account": smallAccount, "amount": 20_000.01}))}, ec)
	if poor.OK || poor.Code != "INSUFFICIENT_BALANCE" {
		t.Errorf("期望 INSUFFICIENT_BALANCE，实际 %s", poor.Code)
	}

	// 预检失败必须停在 decision 阶段，账本和执行次数都不能变化。
	if !maps.Equal(before, SnapshotAccounts()) {
		t.Errorf("预检拒绝时账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("期望 handler 一次都没执行，实际执行了 %d 次", got)
	}
	for _, record := range audit.Records() {
		if record.Phase != "decision" {
			t.Errorf("期望只有 decision 阶段的审计，实际出现了 %s", record.Phase)
		}
	}
}

// TestTransferRequiresApprovalThenExecutesWithRedaction 对应【任务 5 + 任务 6】：
// 高风险写操作先 CONFIRM，审批后执行，结果账号被脱敏。
func TestTransferRequiresApprovalThenExecutesWithRedaction(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, audit := BuildRuntime(nil)
	ec := BaseContext()
	args := transferArgs(map[string]any{"amount": 1200.0})

	fromBefore, _ := Balance(ec.TenantID, fromAccount)
	toBefore, _ := Balance(ec.TenantID, toAccount)

	pending := runtime.Invoke(ctx, ToolCall{"call_pending", "transfer", MustArgs(args)}, ec)
	if pending.Action != ActionConfirm || pending.Code != "APPROVAL_REQUIRED" {
		t.Fatalf("期望 confirm/APPROVAL_REQUIRED，实际 %s/%s", pending.Action, pending.Code)
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("没有审批时不应执行，实际执行了 %d 次", got)
	}

	approvals.Approve("approval_transfer", ec, "transfer", args, 0)
	approved := runtime.Invoke(ctx, ToolCall{"call_done", "transfer", MustArgs(args)},
		ec.WithApprovalID("approval_transfer"))

	if !approved.OK || approved.Action != ActionAllow || approved.Code != "OK" {
		t.Fatalf("期望放行，实际 %s/%s: %v", approved.Action, approved.Code, approved.Content)
	}

	content := contentMap(t, approved)
	if content["txn_id"] != "txn_"+lastN("call_done", 6) {
		t.Errorf("流水号不对：%v", content["txn_id"])
	}
	if content["amount"] != 1200.0 || content["status"] != "succeeded" {
		t.Errorf("返回内容不对：%v", content)
	}

	// 结果脱敏：模型只能看到前缀加后四位。
	if content["from"] != "ACC-A-****3456" || content["to"] != "ACC-A-****4321" {
		t.Errorf("账号未脱敏：%v / %v", content["from"], content["to"])
	}
	if strings.Contains(MarshalResult(approved), fromAccount) {
		t.Errorf("明文账号泄漏到了返回结果里")
	}

	fromAfter, _ := Balance(ec.TenantID, fromAccount)
	toAfter, _ := Balance(ec.TenantID, toAccount)
	if fromAfter != fromBefore-1200 || toAfter != toBefore+1200 {
		t.Errorf("余额变动不对：%v -> %v, %v -> %v", fromBefore, fromAfter, toBefore, toAfter)
	}
	if got := SideEffect("transfer_executions"); got != 1 {
		t.Errorf("期望执行 1 次，实际 %d 次", got)
	}

	// 审计要能还原出"先挂起、再放行、最后执行"这条时间线。
	wanted := map[string]bool{
		"decision/APPROVAL_REQUIRED": false,
		"decision/APPROVED":          false,
		"execution/OK":               false,
	}
	for _, record := range audit.Records() {
		key := record.Phase + "/" + record.Code
		if _, ok := wanted[key]; ok {
			wanted[key] = true
		}
		// 审计只留参数名，不落明文参数值。
		if strings.Contains(strings.Join(record.ArgumentKeys, ","), fromAccount) {
			t.Errorf("审计记录里出现了明文账号")
		}
	}
	for key, seen := range wanted {
		if !seen {
			t.Errorf("审计里缺少 %s", key)
		}
	}
}

// TestTransferApprovalIsBoundToCanonicalArguments 验证审批是一次性的，
// 并且和工具、租户、用户、参数完全绑定。
func TestTransferApprovalIsBoundToCanonicalArguments(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, _ := BuildRuntime(nil)
	ec := BaseContext()

	approvedArgs := transferArgs(map[string]any{"amount": 1000.0})
	approvals.Approve("approval_bound", ec, "transfer", approvedArgs, 0)
	approvedCtx := ec.WithApprovalID("approval_bound")
	before := SnapshotAccounts()

	// 拿着为 1000 元签发的审批去执行 2000 元：摘要对不上，退回 confirm。
	tampered := runtime.Invoke(ctx, ToolCall{"call_tampered", "transfer",
		MustArgs(transferArgs(map[string]any{"amount": 2000.0}))}, approvedCtx)
	if tampered.Action != ActionConfirm || tampered.Code != "APPROVAL_REQUIRED" {
		t.Errorf("改了金额仍被放行：%s/%s", tampered.Action, tampered.Code)
	}
	if !maps.Equal(before, SnapshotAccounts()) {
		t.Errorf("审批不匹配时账本不应变化")
	}

	// 同一份参数，换个人来用这张审批：同样退回 confirm。
	otherUser := runtime.Invoke(ctx, ToolCall{"call_other_user", "transfer", MustArgs(approvedArgs)},
		approvedCtx.WithUserID("u_999"))
	if otherUser.Action != ActionConfirm {
		t.Errorf("换了用户仍被放行：%s", otherUser.Action)
	}

	first := runtime.Invoke(ctx, ToolCall{"call_first", "transfer", MustArgs(approvedArgs)}, approvedCtx)
	if !first.OK {
		t.Fatalf("参数一致时应当放行，实际 %s/%v", first.Code, first.Content)
	}

	// 重放刚刚用过的审批：一次性凭证已作废。
	replayed := runtime.Invoke(ctx, ToolCall{"call_replay", "transfer", MustArgs(approvedArgs)}, approvedCtx)
	if replayed.OK || replayed.Action != ActionConfirm {
		t.Errorf("审批被重复使用：%s/%s", replayed.Action, replayed.Code)
	}
	if got := SideEffect("transfer_executions"); got != 1 {
		t.Errorf("期望只执行 1 次，实际 %d 次", got)
	}
}

// TestTransferTimeoutReportsUnknownResult 对应【任务 4】：
// 非幂等写操作超时后结果未知，且不能留下半成品副作用。
func TestTransferTimeoutReportsUnknownResult(t *testing.T) {
	ctx := setup(t)

	// 单笔限额会先拦住 80000 以上的转账，这里临时放开限额才能走到 handler 的超时分支。
	// 对应 pytest 里的 monkeypatch.setattr。
	originalLimit := SingleTransferLimit
	SingleTransferLimit = 100_000.0
	t.Cleanup(func() { SingleTransferLimit = originalLimit })

	runtime, approvals, audit := BuildRuntime(nil)
	ec := BaseContext()
	args := transferArgs(map[string]any{"amount": 90_000.0})
	approvals.Approve("approval_timeout", ec, "transfer", args, 0)
	before := SnapshotAccounts()

	started := time.Now()
	result := runtime.Invoke(ctx, ToolCall{"call_timeout", "transfer", MustArgs(args)},
		ec.WithApprovalID("approval_timeout"))
	elapsed := time.Since(started)

	if result.OK || result.Action != ActionDeny {
		t.Fatalf("期望超时失败，实际 %s/%s", result.Action, result.Code)
	}
	if result.Code != "TIMEOUT_UNKNOWN" {
		t.Errorf("非幂等写操作超时应报 TIMEOUT_UNKNOWN，实际 %s", result.Code)
	}
	// 超时保护是 1.5 秒，handler 模拟 3 秒。等待时间应当贴近 1.5 秒而不是 3 秒。
	if elapsed > 2*time.Second {
		t.Errorf("超时保护没生效，等了 %v", elapsed)
	}
	// 账本必须一分未动——因为 handler 里的等待排在改余额之前，而且它尊重了 ctx。
	if !maps.Equal(before, SnapshotAccounts()) {
		t.Errorf("超时后账本不应变化")
	}
	if got := SideEffect("transfer_executions"); got != 0 {
		t.Errorf("超时后不应留下副作用，实际执行了 %d 次", got)
	}

	seen := false
	for _, record := range audit.Records() {
		if record.Phase == "execution" && record.Code == "TIMEOUT_UNKNOWN" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("审计里缺少 execution/TIMEOUT_UNKNOWN")
	}
}
