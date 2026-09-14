// 训练营原版的 8 个基线测试，从 Python 版 tests/test_tool_governance.py 前 8 个用例逐个移植。
//
// 每个用例恰好钉住框架的一层，函数名与 Python 版一一对应（snake_case -> CamelCase）。
// 这些用例测的是退款、Shell、查单这些原有工具，与转账作业无关——
// 它们通过，说明加入转账工具没有破坏框架原有的行为。
package governance

import (
	"reflect"
	"testing"
)

// 对应 test_deny_first_beats_bypass_permissions
// Decide 第 1 步：deny 规则排在最前，bypass 模式也盖不掉。
func TestDenyFirstBeatsBypassPermissions(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)

	result := runtime.Invoke(ctx,
		ToolCall{"deny_01", "run_shell", RawArgs(`{"command": "rm -rf /tmp/demo"}`)},
		BaseContext().WithMode(ModeBypassPermissions),
	)

	if result.Code != "DENY_RULE" {
		t.Errorf("期望 DENY_RULE，实际 %s", result.Code)
	}
	if got := SideEffect("shell_executions"); got != 0 {
		t.Errorf("期望 shell 执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_plan_mode_denies_write_before_approval
// Decide 第 2 步：plan 排在审批之前，手握有效审批也照样拒绝写操作。
func TestPlanModeDeniesWriteBeforeApproval(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, _ := BuildRuntime(nil)
	ec := BaseContext().WithMode(ModePlan)
	approvals.Approve("approval_plan", ec, "create_refund", refundArguments(), 0)

	result := runtime.Invoke(ctx,
		ToolCall{"plan_01", "create_refund", MustArgs(refundArguments())},
		ec.WithApprovalID("approval_plan"),
	)

	if result.Code != "PLAN_MODE_DENIED" {
		t.Errorf("期望 PLAN_MODE_DENIED，实际 %s", result.Code)
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("期望退款执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_schema_rejects_forged_identity_and_approval
// 参数校验：DisallowUnknownFields 拦住模型伪造的 user_id 和 approved。
func TestSchemaRejectsForgedIdentityAndApproval(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)

	forged := merge(refundArguments(), map[string]any{"user_id": "admin", "approved": true})
	result := runtime.Invoke(ctx, ToolCall{"schema_01", "create_refund", MustArgs(forged)}, BaseContext())

	if result.Code != "INVALID_ARGUMENT" {
		t.Errorf("期望 INVALID_ARGUMENT，实际 %s", result.Code)
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("期望退款执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_rbac_denial_keeps_handler_at_zero_calls
// Decide 第 4 步：缺少业务权限时，handler 调用次数必须为零。
func TestRBACDenialKeepsHandlerAtZeroCalls(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)

	result := runtime.Invoke(ctx,
		ToolCall{"rbac_01", "create_refund", MustArgs(refundArguments())},
		BaseContext().WithPermissions(NewSet(PermOrderRead)),
	)

	if result.Code != "PERMISSION_DENIED" {
		t.Errorf("期望 PERMISSION_DENIED，实际 %s", result.Code)
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("期望退款执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_approval_is_bound_to_canonical_arguments
// Decide 第 6 步：审批与参数绑定。为 100 元签发的审批，拿去退 399 元会退回 confirm。
func TestApprovalIsBoundToCanonicalArguments(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, _ := BuildRuntime(nil)
	ec := BaseContext()
	approvals.Approve("approval_changed", ec, "create_refund", map[string]any{
		"order_id": "ord_1001",
		"amount":   100.0,
		"reason":   "部分商品退款",
	}, 0)

	result := runtime.Invoke(ctx,
		ToolCall{"approval_01", "create_refund", MustArgs(refundArguments())},
		ec.WithApprovalID("approval_changed"),
	)

	if result.Action != ActionConfirm {
		t.Errorf("期望 confirm，实际 %s", result.Action)
	}
	if result.Code != "APPROVAL_REQUIRED" {
		t.Errorf("期望 APPROVAL_REQUIRED，实际 %s", result.Code)
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("期望退款执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_result_is_redacted_but_audit_keeps_tool_call_id
// finalize 阶段：返回给模型的内容已脱敏，审计里仍保留 tool_call_id 用于追踪。
func TestResultIsRedactedButAuditKeepsToolCallID(t *testing.T) {
	ctx := setup(t)
	runtime, _, audit := BuildRuntime(nil)

	result := runtime.Invoke(ctx,
		ToolCall{"result_01", "get_order", RawArgs(`{"order_id": "ord_1001"}`)},
		BaseContext(),
	)

	if !result.OK {
		t.Fatalf("期望成功，实际 %s: %v", result.Code, result.Content)
	}
	want := map[string]any{
		"status":         "paid",
		"refundable":     399.0,
		"customer_email": "***@***",
		"access_token":   "***",
	}
	// 【Go 差异】Python 直接用 == 比较两个 dict；Go 的 map 不能用 == 比较，
	// 值类型是 any 时用 reflect.DeepEqual。
	if !reflect.DeepEqual(result.Content, want) {
		t.Errorf("返回内容不对\n期望 %v\n实际 %v", want, result.Content)
	}

	records := audit.Records()
	last := records[len(records)-1]
	if last.ToolCallID != "result_01" {
		t.Errorf("审计最后一条的 tool_call_id 期望 result_01，实际 %s", last.ToolCallID)
	}
	if last.Decision != "executed" {
		t.Errorf("审计最后一条的 decision 期望 executed，实际 %s", last.Decision)
	}
}

// 对应 test_discovery_and_execution_both_enforce_whitelist
// 发现期过滤 + Decide 第 3 步执行期重查：模型看不到的工具，硬调也调不动。
func TestDiscoveryAndExecutionBothEnforceWhitelist(t *testing.T) {
	ctx := setup(t)
	runtime, _, _ := BuildRuntime(nil)
	ec := BaseContext().WithAllowedTools(NewSet("get_order"))

	names := toolNames(runtime.ModelTools(ec))
	if !reflect.DeepEqual(names, map[string]bool{"get_order": true}) {
		t.Errorf("发现期应只暴露 get_order，实际 %v", names)
	}

	result := runtime.Invoke(ctx, ToolCall{"stale_01", "create_refund", MustArgs(refundArguments())}, ec)
	if result.Code != "TOOL_NOT_ALLOWED" {
		t.Errorf("期望 TOOL_NOT_ALLOWED，实际 %s", result.Code)
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("期望退款执行 0 次，实际 %d 次", got)
	}
}

// 对应 test_one_time_approval_cannot_be_replayed
// Decide 第 6 步：审批一次性，第二次使用退回 confirm。
func TestOneTimeApprovalCannotBeReplayed(t *testing.T) {
	ctx := setup(t)
	runtime, approvals, _ := BuildRuntime(nil)
	ec := BaseContext()
	approvals.Approve("approval_once", ec, "create_refund", refundArguments(), 0)
	approvedCtx := ec.WithApprovalID("approval_once")

	first := runtime.Invoke(ctx, ToolCall{"once_01", "create_refund", MustArgs(refundArguments())}, approvedCtx)
	second := runtime.Invoke(ctx, ToolCall{"once_02", "create_refund", MustArgs(refundArguments())}, approvedCtx)

	if !first.OK {
		t.Errorf("第一次应当成功，实际 %s", first.Code)
	}
	if second.Action != ActionConfirm || second.Code != "APPROVAL_REQUIRED" {
		t.Errorf("第二次应退回 confirm/APPROVAL_REQUIRED，实际 %s/%s", second.Action, second.Code)
	}
	if got := SideEffect("refund_executions"); got != 1 {
		t.Errorf("期望退款执行 1 次，实际 %d 次", got)
	}
}
