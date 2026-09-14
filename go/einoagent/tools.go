// Package einoagent 用 CloudWeGo Eino 框架重写 Agent Loop，与手写版 governance/agent.go 对照。
//
// 分工：
//
//	Eino        负责"跟模型打交道"：流式请求、tool_calls 拼接、ReAct 循环
//	governance  负责"工具能不能执行"：参数校验、九步权限、审批、脱敏、审计
//
// 两层之间只有一个接口：Eino 工具的 InvokableRun 一律转发给 runtime.Invoke。
// 这是本包最重要的约束——如果把 handler 直接注册成 Eino 工具，
// 模型一调就真的执行了，治理层会被整个绕过。
//
// 本包依赖 Eino，governance 包仍然只依赖标准库。
package einoagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

// GovernedTool 把治理框架里的一个工具适配成 Eino 工具（实现 tool.InvokableTool）。
//
// 注意它身上没有 handler：Eino 能拿到的只有工具的描述，和一个通往 runtime.Invoke 的入口。
type GovernedTool struct {
	definition governance.ToolDefinition
	runtime    *governance.ToolRuntime
	ec         governance.ExecutionContext // 来自认证层，绝不来自模型
	out        io.Writer                   // 打印 [tool_result]，与手写版输出一致；可为 nil
}

var _ tool.InvokableTool = (*GovernedTool)(nil) // 编译期确认实现了接口

// Info 把工具描述投影给 Eino，Eino 再转交给模型。
// 对应手写版里的 ToolDefinition.ToModelTool：只给名字、描述和参数 Schema。
func (g *GovernedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	params, err := toEinoSchema(g.definition.Parameters)
	if err != nil {
		return nil, fmt.Errorf("工具 %s 的参数 Schema 转换失败: %w", g.definition.Name, err)
	}
	return &schema.ToolInfo{
		Name:        g.definition.Name,
		Desc:        g.definition.Description,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(params),
	}, nil
}

// InvokableRun 是 Eino 执行工具时调用的方法。这里不执行任何业务逻辑，只转发给治理框架。
func (g *GovernedTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	return invokeThroughRuntime(ctx, g.runtime, g.ec, g.definition.Name, argumentsInJSON, g.out), nil
}

// invokeThroughRuntime 是 Eino 世界通往治理框架的唯一入口。
//
// 返回值设计成"永远不返回 error"：
// CONFIRM、DENY、TIMEOUT_UNKNOWN 都是正常的治理结论，要作为工具结果交给模型，
// 让模型读到 APPROVAL_REQUIRED、EXCEED_LIMIT 这些错误码去调整下一步。
// 如果返回 error，Eino 会把它当执行失败向上抛，整个 Agent 直接报错结束，模型什么也读不到。
func invokeThroughRuntime(ctx context.Context, runtime *governance.ToolRuntime, ec governance.ExecutionContext,
	name, argumentsInJSON string, out io.Writer) string {

	// 模型给的参数可能不是合法 JSON，与手写版一样包成 _invalid_json 交给参数校验层拒绝。
	arguments := json.RawMessage(argumentsInJSON)
	if !json.Valid(arguments) {
		arguments = governance.MustArgs(map[string]any{"_invalid_json": argumentsInJSON})
	}

	result := runtime.Invoke(ctx, governance.ToolCall{
		// Eino 的 InvokableRun 参数里没有 tool call id，它通过 ctx 传递。
		// 审计记录和幂等键都依赖这个 id，不能丢。
		ToolCallID: compose.GetToolCallID(ctx),
		Name:       name,
		Arguments:  arguments,
	}, ec)

	if out != nil {
		fmt.Fprintf(out, "[tool_result] %s %s\n", result.ToolName, result.Code)
	}
	content, _ := result.ToToolMessage()["content"].(string)
	return content
}

// Toolset 是交给 Eino ToolsNode 的全部配置。
type Toolset struct {
	Tools   []tool.BaseTool
	runtime *governance.ToolRuntime
	ec      governance.ExecutionContext
	out     io.Writer
}

// NewToolset 按执行上下文的白名单挑出工具，适配成 Eino 工具。
//
// 这一步对应手写版的 ModelTools()：发现期过滤，模型只能看到白名单里的工具。
func NewToolset(runtime *governance.ToolRuntime, ec governance.ExecutionContext, out io.Writer) *Toolset {
	// 【与手写版的差异】Eino 的 ToolsNode 默认并发执行同一轮里的多个工具调用，手写版是串行的。
	// 多个工具 goroutine 会同时打印 [tool_result]，所以 writer 必须加锁，
	// 否则写 strings.Builder 这类非并发安全的 writer 会产生数据竞争，写终端时输出行也可能交错。
	// 治理框架本身不受影响：runtime、账本、审批库、审计口早就各自加了锁。
	if out != nil {
		out = &lockedWriter{w: out}
	}
	toolset := &Toolset{runtime: runtime, ec: ec, out: out}
	for _, definition := range governance.BuildTools() {
		if !ec.AllowedTools.Has(definition.Name) {
			continue
		}
		toolset.Tools = append(toolset.Tools, &GovernedTool{
			definition: definition,
			runtime:    runtime,
			ec:         ec,
			out:        out,
		})
	}
	return toolset
}

// ToolsNodeConfig 生成 Eino ToolsNode 的配置。
//
// UnknownToolsHandler 是这里的关键：模型调用一个没注册给 Eino 的工具（比如越权调用 create_refund）时，
// Eino 默认会直接报错，整个 Agent 中断。这里把它也转给 runtime.Invoke，
// 由执行期白名单（Decide 第 3 步）返回 TOOL_NOT_ALLOWED，模型读到后自己改口——
// 这和手写版的行为一致：发现期过滤不是安全边界，执行期必须再查一次。
func (t *Toolset) ToolsNodeConfig() compose.ToolsNodeConfig {
	return compose.ToolsNodeConfig{
		Tools: t.Tools,
		UnknownToolsHandler: func(ctx context.Context, name, input string) (string, error) {
			return invokeThroughRuntime(ctx, t.runtime, t.ec, name, input, t.out), nil
		},
	}
}

// toEinoSchema 把手写的 JSON Schema（map）转成 Eino 使用的 jsonschema.Schema。
// 走一次 JSON 序列化即可，正则、取值范围、additionalProperties:false 这些约束都会保留。
func toEinoSchema(parameters map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	var converted jsonschema.Schema
	if err := json.Unmarshal(raw, &converted); err != nil {
		return nil, err
	}
	return &converted, nil
}

// lockedWriter 让多个 goroutine 可以安全地往同一个 writer 里写，且每次 Write 是完整的一行。
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
