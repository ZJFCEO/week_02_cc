package governance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 十（续）、真实模型 Agent Loop（对应 Python 版 run_deepseek_agent）
// ---------------------------------------------------------------------------
//
// 重点只有一个：模型吐出来的 tool_calls 依然逐个走 runtime.Invoke，治理链路一步都不少。
// 模型只决定"调哪个工具、传什么参数"，它无权决定能不能执行。
//
// 【Go 差异】Python 版用 openai SDK 发请求，SDK 把流式解析和 tool_calls 拼接都藏起来了。
// Go 版不引第三方依赖，直接用 net/http 手写 SSE 解析——这样 SDK 底下在做什么一目了然：
//
//	一次流式响应 = 若干行 "data: {json}"，最后一行 "data: [DONE]"
//	每个 json 里的 delta 只是一小片：文本是一段段 content，工具调用是按 index 分片的 id/name/arguments
//	客户端要做的，就是按 index 把这些碎片拼回完整的工具调用

const (
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekModel   = "deepseek-v4-flash"
	agentSystemPrompt      = "你是订单助手。只根据工具结果回答，不得伪造订单事实。"

	// MaxAgentRounds 是 Agent Loop 的最大轮数，防止模型无限调用工具。
	MaxAgentRounds = 8
)

// AgentConfig 是调用大模型所需的配置。
//
// Python 版直接在函数里读环境变量；Go 版把配置抽出来，测试时才能指向本地的假服务器，
// 不用真的花钱调 DeepSeek。
type AgentConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	HTTPClient *http.Client
}

// AgentConfigFromEnv 按 Python 版同样的规则读取环境变量。
func AgentConfigFromEnv() (AgentConfig, error) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return AgentConfig{}, errors.New("请先设置环境变量 DEEPSEEK_API_KEY")
	}
	return AgentConfig{
		APIKey:  apiKey,
		BaseURL: envOr("DEEPSEEK_BASE_URL", defaultDeepSeekBaseURL),
		Model:   envOr("DEEPSEEK_MODEL", defaultDeepSeekModel),
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// RunDeepSeekAgent 对应 Python 版的 run_deepseek_agent：从环境变量读配置，跑一次真实闭环。
func RunDeepSeekAgent(ctx context.Context, userInput string, out io.Writer) error {
	cfg, err := AgentConfigFromEnv()
	if err != nil {
		return err
	}
	return RunAgent(ctx, cfg, userInput, out)
}

// RunAgent 是 Agent Loop 本体：请求模型 -> 执行工具 -> 把结果塞回对话 -> 再请求模型，最多 8 轮。
func RunAgent(ctx context.Context, cfg AgentConfig, userInput string, out io.Writer) error {
	runtime, _, _ := BuildRuntime(nil)
	// 刻意只放开只读工具：真实模型闭环的演示里不该让它碰到退款和转账。
	ec := BaseContext().WithAllowedTools(NewSet("get_order"))
	client := &chatClient{cfg: cfg}

	// 【Go 差异】对话消息故意用 map 而不是结构体，好和 Python 版的 list[dict] 逐行对照；
	// 而且 ToolResult.ToToolMessage() 返回的就是 map，可以直接塞进来。
	messages := []map[string]any{
		{"role": "system", "content": agentSystemPrompt},
		{"role": "user", "content": userInput},
	}

	for round := 0; round < MaxAgentRounds; round++ {
		// 每一轮都重新调用 ModelTools：发现期白名单在每次请求时生效。
		turn, err := client.streamChat(ctx, messages, runtime.ModelTools(ec), out)
		if err != nil {
			return fmt.Errorf("第 %d 轮请求模型失败: %w", round+1, err)
		}

		assistantMessage := map[string]any{"role": "assistant", "content": turn.text}
		if len(turn.toolCalls) > 0 {
			assistantMessage["tool_calls"] = turn.toolCalls
		}
		messages = append(messages, assistantMessage)

		// 模型没有再调工具，说明它已经给出最终回答，闭环结束。
		if len(turn.toolCalls) == 0 {
			fmt.Fprintln(out)
			return nil
		}

		if turn.text != "" {
			fmt.Fprintln(out)
		}
		for _, call := range turn.toolCalls {
			// 模型给的参数可能根本不是合法 JSON。Python 版把原文包成 {"_invalid_json": ...}
			// 交给 runtime，让参数校验层按 INVALID_ARGUMENT 拒绝——Go 版保持同样的行为。
			arguments := json.RawMessage(call.Function.Arguments)
			if !json.Valid(arguments) {
				arguments = MustArgs(map[string]any{"_invalid_json": call.Function.Arguments})
			}

			// 模型说要调工具，也必须从同一个入口进来，和测试、离线演示走的是同一条主干。
			result := runtime.Invoke(ctx, ToolCall{
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Arguments:  arguments,
			}, ec)
			fmt.Fprintf(out, "[tool_result] %s %s\n", result.ToolName, result.Code)
			messages = append(messages, result.ToToolMessage())
		}
	}

	return fmt.Errorf("Agent Loop 超过最大轮数 %d", MaxAgentRounds)
}

// ---------------------------------------------------------------------------
// 下面是 OpenAI 兼容接口的最小客户端，只实现 Agent Loop 用得到的流式对话
// ---------------------------------------------------------------------------

// providerToolCall 是拼接完成的一次工具调用，字段名与接口协议一致，可以直接回填进对话。
type providerToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatTurn 是一轮流式响应聚合后的结果。
type chatTurn struct {
	text      string
	toolCalls []providerToolCall
}

// streamChunk 是 SSE 里每一行 data 的结构，只声明用得到的字段。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

type chatClient struct {
	cfg AgentConfig
}

// streamChat 发一次流式对话请求，边收边把文本打到 out，同时按 index 拼接工具调用。
func (c *chatClient) streamChat(ctx context.Context, messages []map[string]any, tools []map[string]any, out io.Writer) (chatTurn, error) {
	body, err := json.Marshal(map[string]any{
		"model":    c.cfg.Model,
		"messages": messages,
		"tools":    tools,
		"stream":   true,
		// Python 版通过 extra_body 传这个参数，extra_body 的内容会被合并到请求体顶层。
		"thinking": map[string]any{"type": "disabled"},
	})
	if err != nil {
		return chatTurn{}, err
	}

	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return chatTurn{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	httpClient := c.cfg.HTTPClient
	if httpClient == nil {
		// 不设整体超时：流式响应可能持续很久，靠 ctx 控制取消。
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return chatTurn{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return chatTurn{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var text strings.Builder
	// pending 对应 Python 版的 pending_calls：key 是 tool_call 的 index。
	// 同一次工具调用的 id、name、arguments 会分散在好几个 chunk 里到达，必须按 index 累加。
	pending := map[int]*providerToolCall{}

	scanner := bufio.NewScanner(resp.Body)
	// 默认单行上限 64KB，一个 chunk 里塞了长参数时会读失败，放宽到 1MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 空行是 SSE 事件分隔符，冒号开头是注释（服务端常用来发 keep-alive），都跳过。
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return chatTurn{}, fmt.Errorf("解析流式响应失败: %w", err)
		}
		// 对应 Python 的 `if not chunk.choices: continue`：结尾的用量统计 chunk 没有 choices。
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.Content != "" {
			text.WriteString(delta.Content)
			fmt.Fprint(out, delta.Content) // 边收边打，对应 print(..., end="", flush=True)
		}
		for _, piece := range delta.ToolCalls {
			current, exists := pending[piece.Index]
			if !exists {
				current = &providerToolCall{Type: "function"}
				pending[piece.Index] = current
			}
			if piece.ID != "" {
				current.ID = piece.ID
			}
			// name 和 arguments 都是追加而不是覆盖：它们可能被切成好几段发过来。
			current.Function.Name += piece.Function.Name
			current.Function.Arguments += piece.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		return chatTurn{}, fmt.Errorf("读取流式响应失败: %w", err)
	}

	// 按 index 排序后输出，对应 Python 的 sorted(pending_calls)。
	// 【Go 差异】Go 的 map 遍历顺序是随机的，这里不排序的话，工具执行顺序每次都可能不同。
	indexes := make([]int, 0, len(pending))
	for index := range pending {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	turn := chatTurn{text: text.String()}
	for _, index := range indexes {
		turn.toolCalls = append(turn.toolCalls, *pending[index])
	}
	return turn, nil
}
