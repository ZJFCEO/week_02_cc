package einoagent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino-ext/components/model/deepseek"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

const systemPrompt = "你是订单助手。只根据工具结果回答，不得伪造订单事实。"

// RunFromEnv 按与手写版相同的规则读环境变量（DEEPSEEK_API_KEY 等），跑一次 Eino 版 Agent。
func RunFromEnv(ctx context.Context, userInput string, out io.Writer) error {
	cfg, err := governance.AgentConfigFromEnv()
	if err != nil {
		return err
	}
	return Run(ctx, cfg, userInput, out)
}

// Run 是 Eino 版 Agent Loop。对照手写版 governance.RunAgent，同样的输入、同样的治理、同样的输出格式。
func Run(ctx context.Context, cfg governance.AgentConfig, userInput string, out io.Writer) error {
	runtime, _, _ := governance.BuildRuntime(nil)
	// 与手写版一致：真实模型闭环只放开只读工具。
	ec := governance.BaseContext().WithAllowedTools(governance.NewSet("get_order"))
	toolset := NewToolset(runtime, ec, out)

	agent, err := newAgent(ctx, cfg, toolset, anyChunkHasToolCalls)
	if err != nil {
		return err
	}

	stream, err := agent.Stream(ctx, []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(userInput),
	})
	if err != nil {
		return wrapAgentError(err)
	}
	defer stream.Close()

	// agent.Stream 返回的是最终回答的流；中间轮次的工具调用由 ReAct 图内部处理，
	// [tool_result] 那几行由 GovernedTool 在执行时打印。
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return wrapAgentError(err)
		}
		fmt.Fprint(out, chunk.Content)
	}
	fmt.Fprintln(out)
	return nil
}

// newAgent 组装 ReAct Agent。checker 单独作为参数，测试里可以换成 Eino 的默认实现做对比。
func newAgent(ctx context.Context, cfg governance.AgentConfig, toolset *Toolset,
	checker func(context.Context, *schema.StreamReader[*schema.Message]) (bool, error)) (*react.Agent, error) {

	chatModel, err := deepseek.NewChatModel(ctx, &deepseek.ChatModelConfig{
		APIKey:     cfg.APIKey,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		HTTPClient: cfg.HTTPClient,
		// 对应 Python 版的 extra_body={"thinking": {"type": "disabled"}}。
		ThinkingConfig: &deepseek.ThinkingConfig{Type: "disabled"},
	})
	if err != nil {
		return nil, fmt.Errorf("创建 DeepSeek 模型失败: %w", err)
	}

	return react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chatModel,
		ToolsConfig:      toolset.ToolsNodeConfig(),

		// MaxStep 数的是图里节点的执行步数，不是对话轮数。
		// ReAct 图里一轮 = 模型节点 1 步 + 工具节点 1 步，所以 8 轮对应 16 步：
		// 最多请求模型 8 次、执行工具 8 轮，第 9 次请求模型前就会停下，和手写版的 MaxAgentRounds 对齐。
		MaxStep: governance.MaxAgentRounds * 2,

		// 流式模式下判断"模型这一轮有没有调工具"。Eino 默认只看第一个非空 chunk：
		// 如果模型先说一句"我先查一下"再发 tool_calls，默认实现会误判成"没有调工具"，
		// 把那句话当最终答案直接结束。DeepSeek 真实跑的时候就出现过先说话再调工具的情况，
		// 所以这里换成读完整个流再判断。
		StreamToolCallChecker: checker,
	})
}

// anyChunkHasToolCalls 读完整个流，只要任意一个 chunk 带了 tool_calls 就返回 true。
// 这和手写版的做法一致：手写版本来就是把所有 chunk 收齐再看有没有工具调用。
// Eino 要求检查函数负责关闭流。
func anyChunkHasToolCalls(_ context.Context, sr *schema.StreamReader[*schema.Message]) (bool, error) {
	defer sr.Close()
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if len(msg.ToolCalls) > 0 {
			return true, nil
		}
	}
}

// wrapAgentError 把 Eino 的步数超限错误翻译成和手写版一致的提示。
func wrapAgentError(err error) error {
	if errors.Is(err, compose.ErrExceedMaxSteps) {
		return fmt.Errorf("Agent Loop 超过最大轮数 %d: %w", governance.MaxAgentRounds, err)
	}
	return err
}
