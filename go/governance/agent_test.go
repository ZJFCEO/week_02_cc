// Agent Loop 的测试。
//
// 不连真实的 DeepSeek：用 httptest 在本地起一个假服务器，按 OpenAI 兼容接口的流式协议回数据。
// 这样不花钱、不依赖网络，也能把"模型分片返回 tool_calls -> 拼接 -> 走 runtime -> 结果回填"整条链路测到。
package governance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeModel 是一个脚本化的假模型服务：第 N 次请求返回 script[N] 里的那组 SSE 行。
type fakeModel struct {
	t        *testing.T
	script   []string
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("请求体不是合法 JSON: %v", err)
	}

	f.mu.Lock()
	round := len(f.requests)
	f.requests = append(f.requests, body)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	reply := f.script[len(f.script)-1] // 脚本用完后一直重复最后一段
	if round < len(f.script) {
		reply = f.script[round]
	}
	fmt.Fprint(w, reply)
}

func (f *fakeModel) serve() (*httptest.Server, AgentConfig) {
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)
	return server, AgentConfig{APIKey: "test-key", BaseURL: server.URL, Model: "deepseek-v4-flash"}
}

// sse 把若干个 chunk 编成 SSE 文本，末尾补上 [DONE]。
func sse(chunks ...string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		b.WriteString("data: ")
		b.WriteString(chunk)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func textChunk(text string) string {
	raw, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}},
	})
	return string(raw)
}

func toolChunk(index int, id, name, arguments string) string {
	call := map[string]any{"index": index, "function": map[string]any{}}
	if id != "" {
		call["id"] = id
		call["type"] = "function"
	}
	function := call["function"].(map[string]any)
	if name != "" {
		function["name"] = name
	}
	if arguments != "" {
		function["arguments"] = arguments
	}
	raw, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}},
	})
	return string(raw)
}

// 模型在一轮里发起两个工具调用：一个合法的查单，一个本轮不允许的退款。
// 验证参数分片能正确拼接、查单结果脱敏后才回填给模型、退款被白名单拦下且 handler 一次没执行。
func TestAgentLoopRunsModelToolCallsThroughRuntime(t *testing.T) {
	ctx := setup(t)
	model := &fakeModel{t: t, script: []string{
		// 第 1 轮：先说一句话，再发两个工具调用。查单的参数被故意切成两段发送。
		"data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n" +
			": keep-alive\n\n" + // SSE 注释行，客户端必须忽略
			sse(
				textChunk("我先查一下。"),
				toolChunk(0, "call_order", "get_order", `{"order_`),
				toolChunk(0, "", "", `id": "ord_1001"}`),
				toolChunk(1, "call_refund", "create_refund", `{"order_id":"ord_1001","amount":399,"reason":"模型擅自退款"}`),
				`{"choices":[],"usage":{"total_tokens":42}}`, // 结尾的用量 chunk 没有 choices
			),
		// 第 2 轮：模型读完工具结果，给出最终回答（文本也分两段）。
		sse(textChunk("订单 ord_1001 已支付，"), textChunk("可退 399 元。")),
	}}
	_, cfg := model.serve()

	var out bytes.Buffer
	if err := RunAgent(ctx, cfg, "请查询订单 ord_1001 的状态和可退金额", &out); err != nil {
		t.Fatalf("Agent Loop 返回错误: %v", err)
	}

	// ---------- 终端输出 ----------
	printed := out.String()
	for _, want := range []string{
		"我先查一下。",
		"[tool_result] get_order OK",
		"[tool_result] create_refund TOOL_NOT_ALLOWED",
		"订单 ord_1001 已支付，可退 399 元。",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("输出里缺少 %q\n完整输出:\n%s", want, printed)
		}
	}
	if got := SideEffect("refund_executions"); got != 0 {
		t.Errorf("模型擅自调用的退款不应执行，实际执行了 %d 次", got)
	}

	// ---------- 第 1 次请求：请求格式 ----------
	if len(model.requests) != 2 {
		t.Fatalf("期望请求模型 2 次，实际 %d 次", len(model.requests))
	}
	first := model.requests[0]
	if got := model.headers[0].Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization 头不对: %q", got)
	}
	if first["stream"] != true || first["model"] != "deepseek-v4-flash" {
		t.Errorf("stream/model 不对: %v / %v", first["stream"], first["model"])
	}
	if thinking, _ := first["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Errorf("thinking 应为 disabled，实际 %v", first["thinking"])
	}
	// 发现期白名单：模型只能看到 get_order。
	var toolsSeen []string
	for _, tool := range first["tools"].([]any) {
		toolsSeen = append(toolsSeen, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if strings.Join(toolsSeen, ",") != "get_order" {
		t.Errorf("模型应只看到 get_order，实际 %v", toolsSeen)
	}

	// ---------- 第 2 次请求：回填给模型的对话 ----------
	messages := model.requests[1]["messages"].([]any)
	// system, user, assistant(带 tool_calls), tool(查单), tool(退款)
	if len(messages) != 5 {
		t.Fatalf("第 2 次请求应带 5 条消息，实际 %d 条", len(messages))
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	orderCall := calls[0].(map[string]any)["function"].(map[string]any)
	if orderCall["arguments"] != `{"order_id": "ord_1001"}` {
		t.Errorf("分片参数拼接错误: %v", orderCall["arguments"])
	}

	orderResult := messages[3].(map[string]any)
	if orderResult["tool_call_id"] != "call_order" {
		t.Errorf("工具结果的 tool_call_id 不对: %v", orderResult["tool_call_id"])
	}
	content := orderResult["content"].(string)
	// 最关键的一条：模型拿到的工具结果已经脱敏，明文邮箱和 token 从未进入模型上下文。
	if strings.Contains(content, "alice@example.com") || strings.Contains(content, "tok_demo") {
		t.Errorf("明文敏感信息泄漏给了模型: %s", content)
	}
	if !strings.Contains(content, "***@***") {
		t.Errorf("工具结果里应能看到脱敏后的邮箱: %s", content)
	}

	refundResult := messages[4].(map[string]any)["content"].(string)
	if !strings.Contains(refundResult, "TOOL_NOT_ALLOWED") {
		t.Errorf("退款应被白名单拒绝，回填内容: %s", refundResult)
	}
}

// 模型给的参数不是合法 JSON 时，包成 _invalid_json 交给 runtime，由参数校验层拒绝。
func TestAgentLoopRejectsInvalidJSONArguments(t *testing.T) {
	ctx := setup(t)
	model := &fakeModel{t: t, script: []string{
		sse(toolChunk(0, "call_bad", "get_order", `{"order_id": `)), // 被截断的 JSON
		sse(textChunk("参数有误，请重新提供订单号。")),
	}}
	_, cfg := model.serve()

	var out bytes.Buffer
	if err := RunAgent(ctx, cfg, "查订单", &out); err != nil {
		t.Fatalf("Agent Loop 返回错误: %v", err)
	}
	if !strings.Contains(out.String(), "[tool_result] get_order INVALID_ARGUMENT") {
		t.Errorf("期望 INVALID_ARGUMENT，实际输出:\n%s", out.String())
	}
}

// 模型没完没了地调工具时，第 8 轮之后必须停下来。
func TestAgentLoopStopsAfterMaxRounds(t *testing.T) {
	ctx := setup(t)
	model := &fakeModel{t: t, script: []string{
		sse(toolChunk(0, "call_loop", "get_order", `{"order_id":"ord_1001"}`)),
	}}
	_, cfg := model.serve()

	err := RunAgent(ctx, cfg, "查订单", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "超过最大轮数 8") {
		t.Fatalf("期望超过最大轮数的错误，实际 %v", err)
	}
	if len(model.requests) != MaxAgentRounds {
		t.Errorf("期望请求 %d 次，实际 %d 次", MaxAgentRounds, len(model.requests))
	}
}

// 模型接口返回非 200 时，要把状态码和错误内容带出来，而不是当成空回答。
func TestAgentLoopReportsHTTPError(t *testing.T) {
	ctx := setup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	err := RunAgent(ctx, AgentConfig{APIKey: "bad", BaseURL: server.URL, Model: "m"}, "查订单", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("期望带出 HTTP 401，实际 %v", err)
	}
}

// 没配 API Key 时直接报错，对应 Python 版的 RuntimeError。
func TestRunDeepSeekAgentRequiresAPIKey(t *testing.T) {
	ctx := setup(t)
	t.Setenv("DEEPSEEK_API_KEY", "")

	err := RunDeepSeekAgent(ctx, "查订单", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("期望提示设置 DEEPSEEK_API_KEY，实际 %v", err)
	}
}
