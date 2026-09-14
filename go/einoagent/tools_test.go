package einoagent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

func toolCall(id, name, arguments string) schema.ToolCall {
	return schema.ToolCall{
		ID:       id,
		Type:     "function",
		Function: schema.FunctionCall{Name: name, Arguments: arguments},
	}
}

// 用 Eino 真实的 ToolsNode 执行一条"模型发起了三个工具调用"的消息，不需要模型、不需要网络。
// 验证经过 Eino 之后，治理框架依然是唯一的执行入口。
func TestToolsNodeRoutesEveryCallThroughRuntime(t *testing.T) {
	governance.ResetState()
	t.Cleanup(governance.ResetState)
	ctx := context.Background()

	runtime, _, audit := governance.BuildRuntime(nil)
	ec := governance.BaseContext().WithAllowedTools(governance.NewSet("get_order"))
	// 故意用非并发安全的 strings.Builder：ToolsNode 会并发执行工具，
	// 这个用例在 -race 下能证明 NewToolset 里给 writer 加的锁是必要且有效的。
	var printed strings.Builder
	toolset := NewToolset(runtime, ec, &printed)

	config := toolset.ToolsNodeConfig()
	node, err := compose.NewToolNode(ctx, &config)
	if err != nil {
		t.Fatalf("创建 ToolsNode 失败: %v", err)
	}

	assistant := schema.AssistantMessage("", []schema.ToolCall{
		toolCall("call_order", "get_order", `{"order_id":"ord_1001"}`),
		// 越权：create_refund 没有注册给 Eino，会进 UnknownToolsHandler
		toolCall("call_refund", "create_refund", `{"order_id":"ord_1001","amount":399,"reason":"模型擅自退款"}`),
		// 参数不是合法 JSON
		toolCall("call_bad", "get_order", `{"order_id":`),
	})

	results, err := node.Invoke(ctx, assistant)
	if err != nil {
		t.Fatalf("治理结论不应变成 Eino 的执行错误，实际: %v", err)
	}

	byID := map[string]string{}
	for _, message := range results {
		byID[message.ToolCallID] = message.Content
	}

	// 合法查单：结果已脱敏
	if order := byID["call_order"]; !strings.Contains(order, "***@***") || strings.Contains(order, "alice@example.com") {
		t.Errorf("查单结果应已脱敏，实际: %s", order)
	}
	// 越权调用：Eino 没有报错中断，而是由执行期白名单返回 TOOL_NOT_ALLOWED
	if refund := byID["call_refund"]; !strings.Contains(refund, "TOOL_NOT_ALLOWED") {
		t.Errorf("越权调用应返回 TOOL_NOT_ALLOWED，实际: %s", refund)
	}
	if got := governance.SideEffect("refund_executions"); got != 0 {
		t.Errorf("越权的退款不应执行，实际执行了 %d 次", got)
	}
	// 非法参数：参数校验层拒绝
	if bad := byID["call_bad"]; !strings.Contains(bad, "INVALID_ARGUMENT") {
		t.Errorf("非法参数应返回 INVALID_ARGUMENT，实际: %s", bad)
	}

	// tool call id 通过 ctx 从 Eino 传到了治理框架，审计里每一条都能对上号（包括走 UnknownToolsHandler 的那条）
	seen := map[string]bool{}
	for _, record := range audit.Records() {
		seen[record.ToolCallID] = true
	}
	for _, id := range []string{"call_order", "call_refund", "call_bad"} {
		if !seen[id] {
			t.Errorf("审计里缺少 tool_call_id=%s，说明 compose.GetToolCallID 没有取到", id)
		}
	}

	// 输出格式与手写版一致（ToolsNode 默认并发执行工具，所以只检查包含，不检查顺序）
	for _, line := range []string{"[tool_result] get_order OK", "[tool_result] create_refund TOOL_NOT_ALLOWED", "[tool_result] get_order INVALID_ARGUMENT"} {
		if !strings.Contains(printed.String(), line) {
			t.Errorf("输出缺少 %q，实际:\n%s", line, printed.String())
		}
	}
}

// 高风险工具返回 CONFIRM 时，Eino 拿到的是正常的工具结果，而不是 error。
func TestConfirmIsReturnedAsToolResultNotError(t *testing.T) {
	governance.ResetState()
	t.Cleanup(governance.ResetState)
	ctx := context.Background()

	runtime, _, _ := governance.BuildRuntime(nil)
	ec := governance.BaseContext() // 这里放开全部工具，包括 transfer
	config := NewToolset(runtime, ec, nil).ToolsNodeConfig()
	node, err := compose.NewToolNode(ctx, &config)
	if err != nil {
		t.Fatalf("创建 ToolsNode 失败: %v", err)
	}

	results, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{
		toolCall("call_transfer", "transfer", `{"from_account":"ACC-A-123456","to_account":"ACC-A-654321","amount":1000}`),
	}))
	if err != nil {
		t.Fatalf("CONFIRM 不应变成 error，实际: %v", err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Content, "APPROVAL_REQUIRED") {
		t.Fatalf("期望模型读到 APPROVAL_REQUIRED，实际: %+v", results)
	}
	if got := governance.SideEffect("transfer_executions"); got != 0 {
		t.Errorf("没有审批时转账不应执行，实际 %d 次", got)
	}
}

// 工具描述投影给 Eino 后，参数约束不能丢：模型看到的 Schema 要和手写版完全一致。
func TestGovernedToolInfoKeepsSchemaConstraints(t *testing.T) {
	runtime, _, _ := governance.BuildRuntime(nil)
	toolset := NewToolset(runtime, governance.BaseContext(), nil)

	names := map[string]bool{}
	for _, item := range toolset.Tools {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatalf("Info 失败: %v", err)
		}
		names[info.Name] = true
		if info.Name != "transfer" {
			continue
		}
		js, err := info.ParamsOneOf.ToJSONSchema()
		if err != nil {
			t.Fatalf("转回 JSON Schema 失败: %v", err)
		}
		raw, _ := js.MarshalJSON()
		for _, want := range []string{`"pattern":"^ACC-[A-Z]-[0-9]{6}$"`, `"maximum":100000`, `"additionalProperties":false`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("transfer 的 Schema 缺少约束 %s，实际: %s", want, raw)
			}
		}
	}
	if len(names) != 4 {
		t.Errorf("放开全部工具时应适配 4 个，实际 %v", names)
	}
}
