// Package governance 是 tool_governance_demo.py 的 Go 复刻。
//
// 解决的问题和 Python 版完全一致：模型说"我要调用 transfer"，凭什么让它真的动账本。
// 答案是把每一次工具调用都压到同一条主干上，逐层确认后才允许产生副作用：
//
//	参数校验 -> 权限判断 -> 业务预检 -> 人工审批 -> 执行 -> 超时处理 -> 脱敏 -> 审计
//
// 读代码的入口有两个：
//  1. ToolRuntime.Invoke  —— 主干，一次调用的四个阶段（runtime.go）
//  2. PermissionEngine.Decide —— 九步固定优先级的权限状态机（engine.go）
//
// 和 Python 版的关键差异集中在三处，注释里都标了【Go 差异】：
//   - 参数校验：pydantic -> encoding/json 的 DisallowUnknownFields + 手写 Validate
//   - 超时取消：asyncio 能真的取消协程，Go 不能中断 goroutine，只能放弃等待
//   - 并发安全：Python 单线程事件循环不用锁，Go 的账本和审计必须加锁
package governance

import (
	"context"
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// 一、治理用到的枚举与常量
// ---------------------------------------------------------------------------

// PermissionMode 是调用方所处的模式。它不是一句系统提示词，而是执行层的硬契约。
//
// Go 里没有枚举，惯例是"具名字符串类型 + 一组常量"，这也正是 Python StrEnum 的等价物。
type PermissionMode string

const (
	// ModeDefault 正常模式，高风险动作会返回 confirm 去问人。
	ModeDefault PermissionMode = "default"
	// ModePlan 只读模式，一切写操作和 Shell 直接拒绝。
	ModePlan PermissionMode = "plan"
	// ModeBypassPermissions 跳过"普通确认"，但跳不过 deny 规则、白名单、RBAC 和审批。
	ModeBypassPermissions PermissionMode = "bypassPermissions"
	// ModeDontAsk 非交互模式（CI、定时任务）。没人能点确认，所以该问人时只能拒绝。
	ModeDontAsk PermissionMode = "dontAsk"
)

// Effect 是工具的副作用类型，决定能不能在 plan 模式下执行、超时后能不能自动重试。
type Effect string

const (
	EffectRead  Effect = "read"
	EffectWrite Effect = "write"
	EffectShell Effect = "shell"
)

// Risk 是风险等级。注意 RiskHigh 本身就会触发审批（见 Decide 第 6 步），
// 所以高风险工具的审批是关不掉的，单独把 RequiresApproval 改成 false 没有任何效果。
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// DecisionAction 是权限判断的三种结论。
// Go 里习惯 (bool, error) 两态，这里必须是三态：
//
//	ActionAllow   放行
//	ActionDeny    拒绝，不要再问人，问了也不该给
//	ActionConfirm 还没问人。它不是失败，而是"挂起，等一个绑定本次参数的审批"
type DecisionAction string

const (
	ActionAllow   DecisionAction = "allow"
	ActionDeny    DecisionAction = "deny"
	ActionConfirm DecisionAction = "confirm"
)

// 业务权限常量。Python 用 Literal 限定取值范围（只在静态检查期生效），
// Go 这里用具名常量，同样拦不住有人硬传字符串，靠约定和 code review。
const (
	PermOrderRead    = "order:read"
	PermRefundCreate = "refund:create"
	PermShellRun     = "shell:run"
	PermTransferExec = "transfer:execute"
)

// StringSet 是只读语义的字符串集合，对应 Python 的 frozenset。
//
// 【Go 差异】Go 没有不可变集合。map 是引用类型，即使 ExecutionContext 按值传递，
// 里面的 map 仍然是共享的同一份。这里靠约定：拿到之后只读不写。
type StringSet map[string]struct{}

// NewSet 构造一个集合。
func NewSet(items ...string) StringSet {
	set := make(StringSet, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

// Has 判断成员是否存在。
func (s StringSet) Has(item string) bool {
	_, ok := s[item]
	return ok
}

// ---------------------------------------------------------------------------
// 二、一次调用的上下文与策略
// ---------------------------------------------------------------------------

// ExecutionContext 由认证层生成、工具层只读。
//
// Python 用 @dataclass(frozen=True) 保证不可变；Go 直接按值传递即可——
// handler 拿到的是一份拷贝，改了也影响不到调用方，等价于"拿不到提权的机会"。
type ExecutionContext struct {
	TraceID  string // 全链路追踪 ID，审计记录靠它串起同一次会话
	UserID   string // 操作人。审批凭证会绑定到具体的人，换个人审批即失效
	TenantID string // 租户。所有数据查询都必须带上它，是多租户隔离的唯一依据

	Mode         PermissionMode // 见 PermissionMode
	Permissions  StringSet      // RBAC 权限集合，来自认证层
	AllowedTools StringSet      // 本轮允许的工具名单。发现期要用，执行期还要再查一次
	ApprovalID   string         // 本次调用携带的审批凭证；空字符串表示没有审批
}

// 下面这组 With 方法对应 Python 的 dataclasses.replace。
// 接收者是值类型，所以方法内部改的是副本，返回副本即可——这是 Go 值语义最舒服的地方。

func (c ExecutionContext) WithMode(mode PermissionMode) ExecutionContext {
	c.Mode = mode
	return c
}

func (c ExecutionContext) WithApprovalID(id string) ExecutionContext {
	c.ApprovalID = id
	return c
}

func (c ExecutionContext) WithUserID(id string) ExecutionContext {
	c.UserID = id
	return c
}

func (c ExecutionContext) WithTenantID(id string) ExecutionContext {
	c.TenantID = id
	return c
}

func (c ExecutionContext) WithPermissions(perms StringSet) ExecutionContext {
	c.Permissions = perms
	return c
}

func (c ExecutionContext) WithAllowedTools(tools StringSet) ExecutionContext {
	c.AllowedTools = tools
	return c
}

// ToolPolicy 是一个工具的"危险画像"。七个字段全是治理参数，没有一个是业务参数。
//
// Python 版用位置参数构造，容易数错；Go 这里必须写字段名，可读性白赚。
type ToolPolicy struct {
	Effect           Effect        // 读 / 写 / Shell，决定 plan 模式和重试策略
	Risk             Risk          // 风险等级，RiskHigh 自动要求审批
	Permission       string        // 调用它需要的业务权限，在 Decide 第 4 步比对
	RequiresApproval bool          // 是否强制审批。给"风险不高但仍需人确认"的工具用
	Timeout          time.Duration // 单次执行超时
	MaxRetries       int           // 最大重试次数。只有只读或幂等的工具才会真的重试
	Idempotent       bool          // 是否幂等。决定超时后报 TIMEOUT 还是 TIMEOUT_UNKNOWN
}

// ---------------------------------------------------------------------------
// 三、工具定义与框架内部结构
// ---------------------------------------------------------------------------

// Handler 是真正执行副作用的函数。
//
// 【Go 差异】Python 的签名里没有 error 返回值，错误全靠 raise；
// Go 这里是标准的 (结果, error)，业务拒绝用 *PolicyError 承载错误码。
type Handler func(ctx context.Context, toolCallID string, args Args, ec ExecutionContext) (map[string]any, error)

// Precheck 是副作用发生前的最后一次只读检查，只判断不改状态。
type Precheck func(ctx context.Context, args Args, ec ExecutionContext) error

// CanonicalTarget 把参数压成一个字符串，供 deny/allow 规则做前缀匹配。
type CanonicalTarget func(args Args) string

// ToolDefinition 是一个工具的完整定义：
// 给模型看的部分（Name / Description / Parameters）+ 只给框架看的部分（Policy / Handler / Precheck）。
type ToolDefinition struct {
	Name        string
	Description string

	// NewArgs 每次调用都返回一个全新的空参数对象，用来承载 JSON 解码结果。
	// 【Go 差异】Python 直接拿 pydantic 模型类当工厂，Go 没有这种用法，显式给个构造函数。
	NewArgs func() Args

	// Parameters 是给模型看的 JSON Schema。
	// 【Go 差异】pydantic 能从模型自动生成 schema，Go 只能手写（或上代码生成）。
	Parameters map[string]any

	Policy          ToolPolicy
	Handler         Handler
	Precheck        Precheck
	CanonicalTarget CanonicalTarget
}

// ToModelTool 是投影：这是模型唯一能看到的东西。
// Handler、Policy、Precheck 一律不出现在返回值里，所以模型既不知道有审批这回事，
// 也无法引用到真正执行的函数。
func (t ToolDefinition) ToModelTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		},
	}
}

// PermissionRule 是静态权限规则。
// TargetPrefix 为空表示匹配该工具的所有调用，否则拿 CanonicalTarget(args) 的前缀比对。
type PermissionRule struct {
	Effect       string // "allow" 或 "deny"
	ToolName     string
	TargetPrefix string
}

// PermissionDecision 是一次权限判断的结论。
// Code 是给上游程序判断的机器可读标识，Reason 才是给人看的。
// Source 记录"这个结论是哪一层下的"，排查线上问题时直接定位到 Decide 的第几步。
type PermissionDecision struct {
	Action DecisionAction
	Code   string
	Reason string
	Source string
}

// ToolCall 是模型发过来的一次原始调用。
//
// 【Go 差异】Python 用 dict 承载参数，Go 用 json.RawMessage——这更贴近真实：
// 大模型返回的 tool_calls.function.arguments 本来就是一段 JSON 文本。
type ToolCall struct {
	ToolCallID string
	Name       string
	Arguments  json.RawMessage
}

// ToolResult 是所有分支的统一出口结构。
// 不论成功、拒绝、超时还是异常，都返回它，绝不往外抛 error。
// OK=false 且 Action=ActionConfirm 表示"挂起待确认"，不是失败。
type ToolResult struct {
	ToolCallID string         `json:"tool_call_id"`
	ToolName   string         `json:"tool_name"`
	OK         bool           `json:"ok"`
	Action     DecisionAction `json:"action"`
	Code       string         `json:"code"`
	Content    any            `json:"content"`
	Retryable  bool           `json:"retryable"`
}

// ToToolMessage 把结果转成可以塞回对话的 tool 消息。
func (r ToolResult) ToToolMessage() map[string]any {
	payload, _ := json.Marshal(map[string]any{
		"ok":      r.OK,
		"code":    r.Code,
		"action":  r.Action,
		"content": r.Content,
	})
	return map[string]any{
		"role":         "tool",
		"tool_call_id": r.ToolCallID,
		"content":      string(payload),
	}
}

// AuditRecord 是一条审计记录。
// 注意只记 ArgumentKeys（参数名），不记参数值——审计日志里不该出现明文账号。
// Phase 区分两个阶段：decision（判断结果）与 execution（真的执行了，带耗时）。
type AuditRecord struct {
	TraceID      string   `json:"trace_id"`
	ToolCallID   string   `json:"tool_call_id"`
	ToolName     string   `json:"tool_name"`
	UserID       string   `json:"user_id"`
	TenantID     string   `json:"tenant_id"`
	Phase        string   `json:"phase"`
	Decision     string   `json:"decision"`
	Code         string   `json:"code"`
	ArgumentKeys []string `json:"argument_keys"`
	LatencyMS    int64    `json:"latency_ms"` // decision 阶段固定为 0
}
