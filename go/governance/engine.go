package governance

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 六、权限状态机（对应 Python 版禁止改动的 PermissionEngine.decide）
// ---------------------------------------------------------------------------

// PermissionEngine 是整个框架的核心。它只回答一个问题：这次调用能不能做。
// 至于怎么做，那是 handler 的事，两边通过 PermissionDecision 和 PolicyError 通信。
type PermissionEngine struct {
	rules     []PermissionRule
	approvals *ApprovalStore
}

// NewPermissionEngine 组装权限引擎。
func NewPermissionEngine(rules []PermissionRule, approvals *ApprovalStore) *PermissionEngine {
	return &PermissionEngine{
		rules:     append([]PermissionRule(nil), rules...),
		approvals: approvals,
	}
}

// ruleMatches 先比工具名，再比目标前缀。TargetPrefix 为空表示匹配该工具的全部调用。
func (e *PermissionEngine) ruleMatches(rule PermissionRule, tool ToolDefinition, args Args) bool {
	if rule.ToolName != tool.Name {
		return false
	}
	if rule.TargetPrefix == "" {
		return true
	}
	return strings.HasPrefix(tool.CanonicalTarget(args), rule.TargetPrefix)
}

func (e *PermissionEngine) hasRule(effect string, tool ToolDefinition, args Args) bool {
	for _, rule := range e.rules {
		if rule.Effect == effect && e.ruleMatches(rule, tool, args) {
			return true
		}
	}
	return false
}

// Decide 是执行期授权，返回三态之一，绝不把 error 直接抛给调用方。
//
// 九步的先后顺序是这套设计的主要内容，读的时候对每一步问一句"能不能往上挪或往下挪"：
//
//	1 deny 规则  -> 放最前，所以 bypass 模式也盖不掉硬拒绝
//	2 plan 模式  -> 只读契约落在执行层，不是靠提示词约束模型
//	3 工具白名单 -> 发现期过滤过了，执行期必须再查一次
//	4 RBAC 权限  -> 只认 ExecutionContext，不认模型参数里自称的身份
//	5 业务预检   -> 排在审批之前：余额不够就别去打扰人
//	6 人工审批   -> 高风险写操作的闸门，与参数绑定且一次性
//	7 bypass     -> 排在审批之后：它只能跳过普通确认，跳不过前面任何一道
//	8 allow 规则 -> 永远最后生效，不能反超前面的硬边界
//	9 危险命令   -> 正则兜底，仅对 Shell 类工具
func (e *PermissionEngine) Decide(ctx context.Context, tool ToolDefinition, args Args, ec ExecutionContext) PermissionDecision {
	// -- 第 1 步：deny 优先 ------------------------------------------------
	// 拒绝永远优先于放行，这是所有权限系统的通用前提。
	if e.hasRule("deny", tool, args) {
		return PermissionDecision{ActionDeny, "DENY_RULE", "命中 deny 规则", "rule"}
	}

	// -- 第 2 步：plan 只读模式 --------------------------------------------
	// 把只读契约写在这里，模型就算被提示词注入骗过去了，执行层照样拦得住。
	if ec.Mode == ModePlan && tool.Policy.Effect != EffectRead {
		return PermissionDecision{ActionDeny, "PLAN_MODE_DENIED", "plan 模式禁止写操作和 Shell", "mode"}
	}

	// -- 第 3 步：执行期重查白名单 -----------------------------------------
	// ModelTools() 在发现期已经过滤过一遍了，这里为什么还要查？
	// 因为模型完全可以凭空编一个没给它的工具名调过来，发现期的过滤不是安全边界。
	if !ec.AllowedTools.Has(tool.Name) {
		return PermissionDecision{ActionDeny, "TOOL_NOT_ALLOWED", "工具不在本轮执行白名单", "whitelist"}
	}

	// -- 第 4 步：RBAC 业务权限 --------------------------------------------
	// 比对的是 ec.Permissions（认证层给的），不是参数里的任何字段。
	if !ec.Permissions.Has(tool.Policy.Permission) {
		return PermissionDecision{
			ActionDeny,
			"PERMISSION_DENIED",
			fmt.Sprintf("缺少业务权限 %s", tool.Policy.Permission),
			"rbac",
		}
	}

	// -- 第 5 步：业务预检 -------------------------------------------------
	// 调用工具自己的 Precheck（转账的实现见 transferPrecheck）。
	// 它只允许判断、不允许改状态，是副作用发生前的最后一次只读检查。
	if tool.Precheck != nil {
		if err := tool.Precheck(ctx, args, ec); err != nil {
			var denied *PolicyError
			if errors.As(err, &denied) {
				return PermissionDecision{ActionDeny, denied.Code, denied.Message, "business"}
			}
			// 预检自身出故障时按拒绝处理：宁可不做，也不要带着未知状态往下走。
			return PermissionDecision{ActionDeny, "PRECHECK_ERROR", err.Error(), "business"}
		}
	}

	// -- 第 6 步：人工审批 -------------------------------------------------
	// 注意条件是 RequiresApproval || Risk == RiskHigh，两者是或的关系：
	// 高风险工具的审批关不掉，单独把 RequiresApproval 改成 false 不起任何作用。
	if tool.Policy.RequiresApproval || tool.Policy.Risk == RiskHigh {
		if e.approvals.Consume(ec.ApprovalID, ec, tool.Name, args) {
			return PermissionDecision{ActionAllow, "APPROVED", "审批与当前用户、租户、工具和参数完全匹配", "approval"}
		}
		if ec.Mode == ModeDontAsk {
			// 非交互模式没有人能来点确认，挂起没有意义，只能拒绝。
			return PermissionDecision{ActionDeny, "APPROVAL_REQUIRED", "非交互模式无法完成高风险确认", "approval"}
		}
		return PermissionDecision{ActionConfirm, "APPROVAL_REQUIRED", "需要确认本次具体动作", "approval"}
	}

	// -- 第 7 步：bypass 模式 ----------------------------------------------
	// 位置是重点：它在审批之后。所以 bypassPermissions 能省掉普通确认，
	// 但 deny 规则、plan、白名单、RBAC、预检、审批这六道它一道也绕不过。
	if ec.Mode == ModeBypassPermissions {
		return PermissionDecision{ActionAllow, "BYPASS_ALLOWED", "跳过普通确认，但硬边界已经全部通过", "mode"}
	}

	// -- 第 8 步：allow 规则 -----------------------------------------------
	// allow 是"免确认放行"，不是"提权"。它永远排在所有硬边界之后。
	if e.hasRule("allow", tool, args) {
		return PermissionDecision{ActionAllow, "ALLOW_RULE", "命中 allow 规则", "rule"}
	}

	// -- 第 9 步：危险命令兜底（仅 Shell） ---------------------------------
	// 【Go 差异】Python 用 getattr(arguments, "command", "") 反射取字段；
	// Go 直接类型断言，取不到就说明这不是 Shell 工具的参数。
	if tool.Policy.Effect == EffectShell {
		if shellArgs, ok := args.(*RunShellArgs); ok && isDangerousShell(shellArgs.Command) {
			if ec.Mode == ModeDontAsk {
				return PermissionDecision{ActionDeny, "DANGEROUS_OPERATION", "危险 Shell 在非交互模式下被拒绝", "risk"}
			}
			return PermissionDecision{ActionConfirm, "DANGEROUS_OPERATION", "危险 Shell 需要用户确认", "risk"}
		}
	}

	// -- 兜底：九道检查全部通过，放行 --------------------------------------
	return PermissionDecision{ActionAllow, "DEFAULT_ALLOWED", "所有确定性检查均已通过", "default"}
}
