package einoagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

// fakeDeepSeek 是本地假 DeepSeek 服务：第 N 次请求返回 script[N]，脚本用完后重复最后一段。
type fakeDeepSeek struct {
	mu     sync.Mutex
	script []string
	bodies []string
}

func (f *fakeDeepSeek) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	round := len(f.bodies)
	f.bodies = append(f.bodies, string(raw))
	f.mu.Unlock()

	reply := f.script[len(f.script)-1]
	if round < len(f.script) {
		reply = f.script[round]
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, reply)
}

func (f *fakeDeepSeek) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func startFake(t *testing.T, script ...string) (*fakeDeepSeek, governance.AgentConfig) {
	t.Helper()
	governance.ResetState()
	t.Cleanup(governance.ResetState)
	fake := &fakeDeepSeek{script: script}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, governance.AgentConfig{APIKey: "test-key", BaseURL: server.URL, Model: "deepseek-v4-flash"}
}

func chunk(delta map[string]any, finish any) string {
	raw, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1, "model": "deepseek-v4-flash",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return "data: " + string(raw) + "\n\n"
}

func textDelta(text string) string {
	return chunk(map[string]any{"role": "assistant", "content": text}, nil)
}

func toolDelta(id, name, arguments string) string {
	call := map[string]any{"index": 0, "type": "function", "function": map[string]any{"arguments": arguments}}
	if id != "" {
		call["id"] = id
		call["function"].(map[string]any)["name"] = name
	}
	return chunk(map[string]any{"role": "assistant", "tool_calls": []any{call}}, nil)
}

const done = "data: [DONE]\n\n"

// 模型先说一句话、再发工具调用——真实 DeepSeek 就这样输出过。
// 参数还被切成两段，中间夹一行 SSE 注释。
var talkThenCall = textDelta("我先查一下。") +
	toolDelta("call_order", "get_order", `{"order_`) +
	": keep-alive\n\n" +
	toolDelta("", "", `id":"ord_1001"}`) +
	chunk(map[string]any{}, "tool_calls") + done

var finalAnswer = textDelta("订单 ord_1001 已支付，") + textDelta("可退 399 元。") + chunk(map[string]any{}, "stop") + done

func TestEinoAgentRunsToolsWhenModelTalksBeforeCalling(t *testing.T) {
	fake, cfg := startFake(t, talkThenCall, finalAnswer)

	var out strings.Builder
	if err := Run(context.Background(), cfg, "请查询订单 ord_1001 的状态和可退金额", &out); err != nil {
		t.Fatalf("Agent 返回错误: %v", err)
	}

	printed := out.String()
	for _, want := range []string{"[tool_result] get_order OK", "订单 ord_1001 已支付，可退 399 元。"} {
		if !strings.Contains(printed, want) {
			t.Errorf("输出缺少 %q，实际:\n%s", want, printed)
		}
	}

	bodies := fake.requests()
	if len(bodies) != 2 {
		t.Fatalf("期望请求模型 2 次，实际 %d 次", len(bodies))
	}
	// 请求格式：关掉思考模式、只暴露 get_order
	if !strings.Contains(bodies[0], `"thinking":{"type":"disabled"}`) {
		t.Errorf("第 1 次请求没有关闭思考模式: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `"name":"get_order"`) || strings.Contains(bodies[0], `"name":"transfer"`) {
		t.Errorf("模型应只看到 get_order: %s", bodies[0])
	}
	// 第 2 次请求带上了工具结果，而且是脱敏后的
	if !strings.Contains(bodies[1], `***@***`) || strings.Contains(bodies[1], "alice@example.com") {
		t.Errorf("回填给模型的工具结果应已脱敏: %s", bodies[1])
	}
}

// 对照组：同样的模型输出，用 Eino 默认的 StreamToolCallChecker。
// 默认实现只看第一个非空 chunk，看到"我先查一下。"就判定模型没调工具，直接把这句话当最终答案——
// 工具一次都没执行。这个用例证明 Run 里换成 anyChunkHasToolCalls 是必要的。
func TestEinoDefaultStreamCheckerMissesToolCallsAfterText(t *testing.T) {
	fake, cfg := startFake(t, talkThenCall, finalAnswer)
	ctx := context.Background()

	runtime, _, audit := governance.BuildRuntime(nil)
	ec := governance.BaseContext().WithAllowedTools(governance.NewSet("get_order"))
	agent, err := newAgent(ctx, cfg, NewToolset(runtime, ec, nil), nil) // nil = 用 Eino 默认检查函数
	if err != nil {
		t.Fatalf("创建 Agent 失败: %v", err)
	}

	stream, err := agent.Stream(ctx, []*schema.Message{schema.UserMessage("查订单")})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	var answer strings.Builder
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读取流失败: %v", err)
		}
		answer.WriteString(msg.Content)
	}
	stream.Close()

	if got := len(fake.requests()); got != 1 {
		t.Errorf("默认检查函数下只会请求 1 次模型，实际 %d 次", got)
	}
	if got := len(audit.Records()); got != 0 {
		t.Errorf("默认检查函数下工具不会被执行，实际审计有 %d 条", got)
	}
	if !strings.HasPrefix(answer.String(), "我先查一下。") {
		t.Errorf("默认检查函数会把开场白当最终答案，实际: %q", answer.String())
	}
}

// 模型没完没了地调工具：MaxStep=16 应当正好对应最多请求 8 次模型，与手写版一致。
func TestEinoAgentStopsAfterMaxRounds(t *testing.T) {
	loop := toolDelta("call_loop", "get_order", `{"order_id":"ord_1001"}`) + chunk(map[string]any{}, "tool_calls") + done
	fake, cfg := startFake(t, loop)

	err := Run(context.Background(), cfg, "查订单", io.Discard)
	if err == nil {
		t.Fatal("期望超过最大轮数报错，实际正常结束")
	}
	if !errors.Is(err, compose.ErrExceedMaxSteps) || !strings.Contains(err.Error(), "超过最大轮数 8") {
		t.Errorf("错误信息不对: %v", err)
	}
	if got := len(fake.requests()); got != governance.MaxAgentRounds {
		t.Errorf("期望请求模型 %d 次（与手写版一致），实际 %d 次", governance.MaxAgentRounds, got)
	}
}
