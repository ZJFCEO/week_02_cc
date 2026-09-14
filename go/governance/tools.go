package governance

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 八、模拟数据与工具实现
// ---------------------------------------------------------------------------

// AccountKey 是账本的键：租户 + 账号。
//
// 【Go 差异】Python 直接用元组 ("tenant_a", "ACC-A-123456") 当字典键；
// Go 用可比较的 struct 当 map 键，效果一样，而且字段有名字更清楚。
// 这样每次查询都被迫带上租户，天然隔离。
type AccountKey struct {
	Tenant  string
	Account string
}

// SingleTransferLimit 是单笔转账限额。
//
// 抽成包级变量而不是写死在函数里，测试才能临时放开它，
// 去验证被限额挡住的超时分支（见 TestTransferTimeoutReportsUnknownResult）。
var SingleTransferLimit = 50_000.0

// 【Go 差异】Python 版的这些全局状态没有锁，因为 asyncio 是单线程事件循环。
// Go 的工具可能被多个 goroutine 并发调用，go test 也可能并行跑用例，所以必须上锁。
// 真实系统里这层锁的位置应该是数据库事务。
var (
	ledgerMu sync.Mutex

	orders = map[AccountKey]map[string]any{
		{Tenant: "tenant_a", Account: "ord_1001"}: {
			"status":         "paid",
			"refundable":     399.0,
			"customer_email": "alice@example.com",
		},
	}

	// accounts 是【作业·任务 1】的模拟账户余额。
	// 注意 tenant_b 的账户对 tenant_a 来说等于不存在——跨租户转账会直接落到余额不足分支。
	accounts = map[AccountKey]float64{
		{Tenant: "tenant_a", Account: "ACC-A-123456"}: 100_000.0,
		{Tenant: "tenant_a", Account: "ACC-A-654321"}: 5_000.0,
		{Tenant: "tenant_a", Account: "ACC-A-888888"}: 20_000.0,
		{Tenant: "tenant_b", Account: "ACC-B-111111"}: 50_000.0,
	}

	initialAccounts = maps.Clone(accounts)

	// sideEffects 是副作用计数器，测试靠它断言"被拒绝的调用真的一次都没执行"。
	sideEffects = map[string]int{}
)

// ResetState 复位账本和副作用计数器，供测试与演示在开始前调用。
func ResetState() {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	accounts = maps.Clone(initialAccounts)
	sideEffects = map[string]int{}
}

// Balance 查询余额。第二个返回值表示该账户在这个租户下是否存在。
func Balance(tenant, account string) (float64, bool) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	value, ok := accounts[AccountKey{Tenant: tenant, Account: account}]
	return value, ok
}

// SnapshotAccounts 返回账本快照，测试用它断言"余额一分没动"。
func SnapshotAccounts() map[AccountKey]float64 {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	return maps.Clone(accounts)
}

// SideEffect 返回某个计数器的当前值。
func SideEffect(name string) int {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	return sideEffects[name]
}

// --------------------------- 工具实现：查询订单（只读） ---------------------------

func getOrderHandler(_ context.Context, _ string, args Args, ec ExecutionContext) (map[string]any, error) {
	// 运行时窄化类型。Handler 签名收的是 Args 接口，这一步相当于 Python 的 assert isinstance。
	typed, ok := args.(*GetOrderArgs)
	if !ok {
		return nil, Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	ledgerMu.Lock()
	order, exists := orders[AccountKey{Tenant: ec.TenantID, Account: typed.OrderID}]
	ledgerMu.Unlock()
	if !exists {
		return nil, Denied("ORDER_NOT_FOUND", "当前租户下不存在该订单")
	}

	out := maps.Clone(order)
	// 故意塞一个 token，用来演示 Redact 会把它换成 ***。
	out["access_token"] = "tok_demo_should_not_leak"
	return out, nil
}

// --------------------------- 工具实现：退款（高风险写） ---------------------------

func refundPrecheck(_ context.Context, args Args, ec ExecutionContext) error {
	typed, ok := args.(*CreateRefundArgs)
	if !ok {
		return Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	ledgerMu.Lock()
	order, exists := orders[AccountKey{Tenant: ec.TenantID, Account: typed.OrderID}]
	ledgerMu.Unlock()
	if !exists || order["status"] != "paid" {
		return Denied("BUSINESS_RULE_DENIED", "订单不存在或状态不可退款")
	}
	refundable, _ := order["refundable"].(float64)
	if typed.Amount > refundable {
		return Denied("BUSINESS_RULE_DENIED", "退款金额超过可退金额")
	}
	return nil
}

func createRefundHandler(_ context.Context, toolCallID string, args Args, ec ExecutionContext) (map[string]any, error) {
	typed, ok := args.(*CreateRefundArgs)
	if !ok {
		return nil, Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	ledgerMu.Lock()
	sideEffects["refund_executions"]++
	ledgerMu.Unlock()

	return map[string]any{
		"refund_id": "ref_9001",
		// 真实实现里这里会调支付网关，所以把 toolCallID 当幂等键传下去。
		"idempotency_key": toolCallID,
		"tenant_id":       ec.TenantID,
		"order_id":        typed.OrderID,
		"amount":          typed.Amount,
		"status":          "accepted",
	}, nil
}

// --------------------------- 工具实现：模拟 Shell ---------------------------

func simulatedShellHandler(_ context.Context, _ string, args Args, _ ExecutionContext) (map[string]any, error) {
	typed, ok := args.(*RunShellArgs)
	if !ok {
		return nil, Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	ledgerMu.Lock()
	sideEffects["shell_executions"]++
	ledgerMu.Unlock()

	return map[string]any{
		"simulated": true,
		"command":   typed.Command,
		"stdout":    "教学模拟：没有创建真实子进程",
	}, nil
}

// --------------------------- 工具实现：转账（本次作业） ---------------------------

// transferPrecheck 是【作业·任务 3】的业务预检。三条纪律：
//
//  1. 只判断，不改任何状态——它在 Decide 第 5 步跑，此时还没到执行阶段
//  2. 有先后顺序：先限额后余额，因为限额是政策问题，余额是资金问题
//  3. 每个拒绝都带机器可读的错误码，模型看到 EXCEED_LIMIT 会改金额，
//     看到 INSUFFICIENT_BALANCE 会换账户——给一句中文描述它只能瞎猜
func transferPrecheck(_ context.Context, args Args, ec ExecutionContext) error {
	typed, ok := args.(*TransferArgs)
	if !ok {
		return Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	// 1. 单笔限额：额度是业务边界，必须在 handler 之前拦截。
	if typed.Amount > SingleTransferLimit {
		return Denied("EXCEED_LIMIT", "单笔转账金额超过限额")
	}

	// 2. 余额充足：只读校验，不改账本。
	// 查余额必须带 ec.TenantID：拿 A 租户的身份查 B 租户的账号，这里会返回 false，
	// 这就是跨租户转账被挡住的地方，不需要额外写一条租户校验。
	balance, exists := Balance(ec.TenantID, typed.FromAccount)
	if !exists || balance < typed.Amount {
		return Denied("INSUFFICIENT_BALANCE", "转出账户余额不足")
	}
	return nil
}

// transferHandler 是【作业·任务 4】的转账执行，全篇唯一允许产生副作用的地方。
//
// 三处顺序都是有意安排的，改了顺序就会出事：
//  1. 超时模拟排在最前：超时发生时账本还没动，所以钱是安全的
//  2. 转入账户检查排在扣款之前：否则会出现"钱扣了但没人收"
//  3. 计数器和两笔账在同一把锁里：保证它和真实副作用一致
//
// 【Go 差异】注意这里的 select：Go 不能从外部中断 goroutine，
// handler 必须自己盯着 ctx.Done()，否则运行时的超时只是"不再等它"，
// 这个函数照样会把钱扣掉——那才是真正的 TIMEOUT_UNKNOWN。
func transferHandler(ctx context.Context, toolCallID string, args Args, ec ExecutionContext) (map[string]any, error) {
	typed, ok := args.(*TransferArgs)
	if !ok {
		return nil, Denied("ARGUMENT_TYPE_MISMATCH", "参数类型不匹配")
	}

	if typed.Amount > 80_000 {
		// 教学用超时模拟：等 3 秒，而工具策略里的超时是 1.5 秒，必然超时。
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			// 尊重取消信号，直接返回，下面的账本操作一行都不会执行。
			return nil, ctx.Err()
		}
	}

	fromKey := AccountKey{Tenant: ec.TenantID, Account: typed.FromAccount}
	toKey := AccountKey{Tenant: ec.TenantID, Account: typed.ToAccount}

	ledgerMu.Lock()
	defer ledgerMu.Unlock()

	if _, exists := accounts[toKey]; !exists {
		// 转入账户不存在（含跨租户的情况）。此时一分钱都还没动，直接返回业务拒绝。
		return nil, Denied("ACCOUNT_NOT_FOUND", "转入账户不存在")
	}

	// 真正的副作用：一扣一加。真实系统里这两行必须在一个数据库事务里。
	accounts[fromKey] -= typed.Amount
	accounts[toKey] += typed.Amount
	sideEffects["transfer_executions"]++

	return map[string]any{
		// 用 toolCallID 后 6 位当流水号。返回的是明文账号，脱敏由 runtime 统一负责。
		"txn_id": fmt.Sprintf("txn_%s", lastN(toolCallID, 6)),
		"from":   typed.FromAccount,
		"to":     typed.ToAccount,
		"amount": typed.Amount,
		"status": "succeeded",
	}, nil
}

func lastN(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[len(runes)-n:])
}

// ---------------------------------------------------------------------------
// 九、工具注册与运行时组装
// ---------------------------------------------------------------------------

// BuildTools 是把工具接进框架的唯一入口。
//
// 【Go 差异】Python 用位置参数构造 ToolPolicy，数错一个位置就出事；
// Go 必须写字段名，这一点白赚。代价是 Parameters 那张 JSON Schema 得手写，
// 不像 pydantic 能从模型自动生成。
func BuildTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "get_order",
			Description: "查询当前租户订单状态和可退金额",
			NewArgs:     func() Args { return &GetOrderArgs{} },
			Parameters: objectSchema(map[string]any{
				"order_id": map[string]any{"type": "string", "pattern": `^ord_[0-9]{4}$`},
			}, "order_id"),
			Policy: ToolPolicy{
				// 只读 + 幂等，所以允许重试 2 次，超时后报的是可安全重试的 TIMEOUT。
				Effect: EffectRead, Risk: RiskMedium, Permission: PermOrderRead,
				RequiresApproval: false, Timeout: time.Second, MaxRetries: 2, Idempotent: true,
			},
			Handler:         getOrderHandler,
			CanonicalTarget: func(args Args) string { return args.(*GetOrderArgs).OrderID },
		},
		{
			Name:        "create_refund",
			Description: "为当前租户的已支付订单创建退款",
			NewArgs:     func() Args { return &CreateRefundArgs{} },
			Parameters: objectSchema(map[string]any{
				"order_id": map[string]any{"type": "string", "pattern": `^ord_[0-9]{4}$`},
				"amount":   map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 10000},
				"reason":   map[string]any{"type": "string", "minLength": 4, "maxLength": 200},
			}, "order_id", "amount", "reason"),
			Policy: ToolPolicy{
				Effect: EffectWrite, Risk: RiskHigh, Permission: PermRefundCreate,
				RequiresApproval: true, Timeout: 2 * time.Second, MaxRetries: 0, Idempotent: false,
			},
			Handler:  createRefundHandler,
			Precheck: refundPrecheck,
			CanonicalTarget: func(args Args) string {
				typed := args.(*CreateRefundArgs)
				return fmt.Sprintf("%s:%v", typed.OrderID, typed.Amount)
			},
		},
		{
			Name:        "run_shell",
			Description: "教学用模拟 Shell，不执行真实系统命令",
			NewArgs:     func() Args { return &RunShellArgs{} },
			Parameters: objectSchema(map[string]any{
				"command": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
			}, "command"),
			Policy: ToolPolicy{
				Effect: EffectShell, Risk: RiskMedium, Permission: PermShellRun,
				RequiresApproval: false, Timeout: time.Second, MaxRetries: 0, Idempotent: false,
			},
			Handler:         simulatedShellHandler,
			CanonicalTarget: func(args Args) string { return args.(*RunShellArgs).Command },
		},
		{
			// 【作业·任务 5】转账工具的注册。
			Name:        "transfer",
			Description: "在当前租户的账户之间发起一笔转账",
			NewArgs:     func() Args { return &TransferArgs{} },
			Parameters: objectSchema(map[string]any{
				"from_account": map[string]any{"type": "string", "pattern": `^ACC-[A-Z]-[0-9]{6}$`},
				"to_account":   map[string]any{"type": "string", "pattern": `^ACC-[A-Z]-[0-9]{6}$`},
				"amount":       map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 100000},
			}, "from_account", "to_account", "amount"),
			Policy: ToolPolicy{
				Effect:           EffectWrite,             // 写操作，plan 模式下直接拒绝
				Risk:             RiskHigh,                // 高风险，自动触发审批（这一项就足够了）
				Permission:       PermTransferExec,        // 缺这个权限停在 Decide 第 4 步
				RequiresApproval: true,                    // 显式要求审批，写出来是为了表达意图
				Timeout:          1500 * time.Millisecond, // 小于 handler 里模拟的 3 秒
				MaxRetries:       0,                       // 不重试：非幂等的转账重试一次就是重复扣款
				Idempotent:       false,                   // 非幂等，超时报 TIMEOUT_UNKNOWN
			},
			Handler:  transferHandler,
			Precheck: transferPrecheck,
			// 规范化目标：转出->转入，供 deny/allow 规则做前缀匹配。
			// 不含金额也不影响审批安全：审批摘要绑定的是完整参数，这里只服务于规则匹配。
			CanonicalTarget: func(args Args) string {
				typed := args.(*TransferArgs)
				return fmt.Sprintf("%s->%s", typed.FromAccount, typed.ToAccount)
			},
		},
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
		// additionalProperties: false 就是 Python 里 extra="forbid" 在 Schema 上的投影。
		"additionalProperties": false,
	}
}

// DefaultRules 是静态权限规则表。
// 前两条是 deny（Decide 第 1 步命中，任何模式都盖不掉），
// 第三条 allow 让 pytest 免确认放行（第 8 步才生效，排在所有硬边界之后）。
var DefaultRules = []PermissionRule{
	{Effect: "deny", ToolName: "run_shell", TargetPrefix: "rm -rf"},
	{Effect: "deny", ToolName: "run_shell", TargetPrefix: "git push --force"},
	{Effect: "allow", ToolName: "run_shell", TargetPrefix: "go test"},
}

// BaseContext 构造一个演示/测试用的执行上下文。
// 真实系统里这个对象必须由认证层生成，绝不能让调用方自己拼。
func BaseContext() ExecutionContext {
	return ExecutionContext{
		TraceID:  "trace_demo",
		UserID:   "u_100",
		TenantID: "tenant_a",
		Mode:     ModeDefault,
		Permissions: NewSet(
			PermOrderRead, PermRefundCreate, PermShellRun, PermTransferExec,
		),
		AllowedTools: NewSet("get_order", "create_refund", "run_shell", "transfer"),
	}
}

// BuildRuntime 组装运行时：规则表 + 审批库 -> 权限引擎 -> 加上工具列表和审计口。
func BuildRuntime(rules []PermissionRule) (*ToolRuntime, *ApprovalStore, *AuditSink) {
	if rules == nil {
		rules = DefaultRules
	}
	approvals := NewApprovalStore()
	sink := NewAuditSink()
	engine := NewPermissionEngine(rules, approvals)
	return NewToolRuntime(BuildTools(), engine, sink), approvals, sink
}
