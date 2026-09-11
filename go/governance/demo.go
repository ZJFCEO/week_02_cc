package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// 十、离线演示
// ---------------------------------------------------------------------------

// RunOfflineDemo 是不连大模型的完整演示，九次调用覆盖治理链路的主要分支：
//
//	call_01 只读放行            call_02 高风险缺审批 -> confirm
//	call_03 带审批执行成功      call_04 模型注入越权字段 -> INVALID_ARGUMENT
//	call_05 危险命令命中 deny   call_06 plan 模式拒绝写操作
//	call_07 转账缺审批 -> confirm
//	call_08 转账带审批成功（返回的账号已脱敏）
//	call_09 同一张审批换了金额 -> 先被业务预检的 EXCEED_LIMIT 挡住
func RunOfflineDemo(ctx context.Context) {
	ResetState()
	runtime, approvals, audit := BuildRuntime(nil)
	ec := BaseContext()

	refundArgs := map[string]any{
		"order_id": "ord_1001",
		"amount":   399.0,
		"reason":   "商品存在质量问题",
	}
	transferArgs := map[string]any{
		"from_account": "ACC-A-123456",
		"to_account":   "ACC-A-654321",
		"amount":       1200.0,
	}

	var results []ToolResult
	results = append(results,
		runtime.Invoke(ctx, ToolCall{"call_01", "get_order", RawArgs(`{"order_id":"ord_1001"}`)}, ec),
		runtime.Invoke(ctx, ToolCall{"call_02", "create_refund", MustArgs(refundArgs)}, ec),
	)

	approvals.Approve("approval_01", ec, "create_refund", refundArgs, 0)
	results = append(results,
		runtime.Invoke(ctx, ToolCall{"call_03", "create_refund", MustArgs(refundArgs)}, ec.WithApprovalID("approval_01")),
		runtime.Invoke(ctx, ToolCall{"call_04", "create_refund", MustArgs(withExtra(refundArgs))}, ec),
		runtime.Invoke(ctx, ToolCall{"call_05", "run_shell", RawArgs(`{"command":"rm -rf /tmp/demo"}`)}, ec.WithMode(ModeBypassPermissions)),
		runtime.Invoke(ctx, ToolCall{"call_06", "create_refund", MustArgs(refundArgs)}, ec.WithMode(ModePlan).WithApprovalID("approval_01")),
		runtime.Invoke(ctx, ToolCall{"call_07", "transfer", MustArgs(transferArgs)}, ec),
	)

	approvals.Approve("approval_02", ec, "transfer", transferArgs, 0)
	results = append(results,
		runtime.Invoke(ctx, ToolCall{"call_08", "transfer", MustArgs(transferArgs)}, ec.WithApprovalID("approval_02")),
		runtime.Invoke(ctx, ToolCall{"call_09", "transfer", MustArgs(withAmount(transferArgs, 60_000))}, ec.WithApprovalID("approval_02")),
	)

	for _, result := range results {
		fmt.Println(MarshalResult(result))
	}

	summary, _ := json.Marshal(map[string]any{
		"side_effects": map[string]int{
			"refund_executions":   SideEffect("refund_executions"),
			"shell_executions":    SideEffect("shell_executions"),
			"transfer_executions": SideEffect("transfer_executions"),
		},
		"audit_records": len(audit.Records()),
	})
	fmt.Println(string(summary))

	// 打印账本，账号同样走脱敏。
	snapshot := SnapshotAccounts()
	keys := make([]AccountKey, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Tenant != keys[j].Tenant {
			return keys[i].Tenant < keys[j].Tenant
		}
		return keys[i].Account < keys[j].Account
	})
	balances := make(map[string]float64, len(keys))
	for _, key := range keys {
		balances[key.Tenant+"/"+RedactString(key.Account)] = snapshot[key]
	}
	balanceLine, _ := json.Marshal(map[string]any{"balances": balances})
	fmt.Println(string(balanceLine))

	for _, record := range audit.Records() {
		line, _ := json.Marshal(record)
		fmt.Println(string(line))
	}
}

func withExtra(base map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	// 模型试图注入越权字段，会被 DisallowUnknownFields 挡住。
	out["approved"] = true
	return out
}

func withAmount(base map[string]any, amount float64) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	out["amount"] = amount
	return out
}
