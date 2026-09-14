"""工具治理与权限状态机教学示例。

这个文件解决的问题：模型说"我要调用 transfer"，凭什么让它真的动账本。
答案是把每一次工具调用都压到同一条主干上，逐层确认后才允许产生副作用：

    参数校验 -> 权限判断 -> 业务预检 -> 人工审批 -> 执行 -> 超时处理 -> 脱敏 -> 审计

读代码的入口有两个，其余都是配角：
    1. ToolRuntime.invoke  —— 主干，一次调用的四个阶段
    2. PermissionEngine.decide —— 九步固定优先级的权限状态机

给 Go 背景的读者的语法对照（下文不再重复解释）：

    Python                          Go 里大致对应什么
    ---------------------------     -------------------------------------------
    @dataclass                      struct，自动生成构造函数与 __eq__
    frozen=True                     不可变；任何赋值都抛异常，等于只传值不传指针
    slots=True                      固定字段布局，省内存，且禁止运行时动态加字段
    class X(StrEnum)                type X string + const 常量组
    Literal["a", "b"]               没有对应物；只在静态检查期生效，运行时不拦
    X | None                        *X，可能为空
    frozenset[str]                  只读的 map[string]struct{}
    Mapping[str, Any]               只读视图，约等于传 map 但声明"我不会改你"
    Callable[[A], Awaitable[B]]     type F func(A) (B, error)，但错误靠 raise 传
    async def / await               协程；单线程事件循环，await 处才让出执行权
    raise / try-except              Go 里 if err != nil 的位置，这里全是异常
    dataclasses.replace(x, k=v)     复制一份 struct 再改字段（因为原件不可变）
"""

# 让本文件所有类型注解延迟求值（当成字符串存着）。
# 作用是允许在类型尚未定义完时就引用它，例如下方 ToolDefinition 里引用后面才定义的类型。
from __future__ import annotations

# 标准库。Python 没有 go.mod，依赖靠外部的 uv/pip 管理，import 只管引用。
import argparse
import asyncio
import hashlib
import json
import os
import re
import time
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass, field, replace
from enum import StrEnum
from typing import Any, Literal


# pydantic 是第三方库：运行时做结构体校验，相当于 Go 的 encoding/json + validator 标签，
# 但校验规则直接写在字段上，并且在运行时强制执行（不是只做静态检查）。
from pydantic import BaseModel, ConfigDict, Field, ValidationError


# =========================== 一、治理用到的枚举与常量 ===========================

# 调用方所处的模式。它不是一句系统提示词，而是执行层的硬契约（见 decide 第 2、6、7 步）：
#     DEFAULT            正常模式，高风险动作会返回 confirm 去问人
#     PLAN               只读模式，一切写操作和 Shell 直接拒绝
#     BYPASS_PERMISSIONS 跳过"普通确认"，但跳不过 deny 规则、白名单、RBAC 和审批
#     DONT_ASK           非交互模式（CI、定时任务）。没人能点确认，所以该问人时只能拒绝
class PermissionMode(StrEnum):
    DEFAULT = "default"
    PLAN = "plan"
    BYPASS_PERMISSIONS = "bypassPermissions"
    DONT_ASK = "dontAsk"



# 工具的副作用类型。决定两件事：能不能在 plan 模式下执行、超时后能不能自动重试。
class Effect(StrEnum):
    READ = "read"
    WRITE = "write"
    SHELL = "shell"



# 风险等级。注意 HIGH 本身就会触发审批（decide 第 6 步是 requires_approval or risk is HIGH），
# 所以高风险工具的审批是关不掉的，单独把 requires_approval 改成 False 没有任何效果。
class Risk(StrEnum):
    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"



# 权限判断的三种结论。Go 里习惯 (bool, error) 两态，这里必须是三态：
#     ALLOW   放行
#     DENY    拒绝，不要再问人，问了也不该给
#     CONFIRM 还没问人。它不是失败，而是"挂起，等一个绑定本次参数的审批"
class DecisionAction(StrEnum):
    ALLOW = "allow"
    DENY = "deny"
    CONFIRM = "confirm"



# 业务权限的取值范围。Literal 只在静态检查（mypy）时生效，运行时不会拦住别的字符串——
# 这一点 Go 的类型系统更强，这里只能靠约定和 code review。
Permission = Literal["order:read", "refund:create", "shell:run", "transfer:execute"]


@dataclass(frozen=True, slots=True)
# =========================== 二、一次调用的上下文与策略 ===========================

# 由认证层生成、工具层只读的执行上下文。frozen=True 意味着 handler 拿到它也无法给自己提权。
# 整个框架只相信这个对象，绝不相信模型在参数里自称的身份（见 run_offline_demo 的 call_04）。
class ExecutionContext:
    # 全链路追踪 ID，审计记录靠它串起同一次会话
    trace_id: str
    # 操作人。审批凭证会绑定到具体的人，换个人审批即失效
    user_id: str
    # 租户。所有数据查询都必须带上它，是多租户隔离的唯一依据
    tenant_id: str
    # 见上方 PermissionMode
    mode: PermissionMode
    # RBAC 权限集合，来自认证层，不可变
    permissions: frozenset[Permission]
    # 本轮允许的工具名单。发现期要用它过滤，执行期还要再查一次
    allowed_tools: frozenset[str]
    # 本次调用携带的审批凭证；None 表示没有审批
    approval_id: str | None = None


@dataclass(frozen=True, slots=True)

# 一个工具的"危险画像"。七个字段全是治理参数，没有一个是业务参数。
# 新增工具时，作者主要就是在填这张表——填完，审批、超时、重试、审计全部自动生效。
class ToolPolicy:
    # 读 / 写 / Shell，决定 plan 模式和重试策略
    effect: Effect
    # 风险等级，HIGH 自动要求审批
    risk: Risk
    # 调用它需要的业务权限，在 decide 第 4 步比对
    permission: Permission
    # 是否强制审批。给"风险不高但仍需人确认"的工具用
    requires_approval: bool
    # 单次执行超时。转账设 2.0 秒，handler 里 sleep 3 秒必然超时
    timeout_seconds: float
    # 最大重试次数。只有只读或幂等的工具才会真的重试
    max_retries: int
    # 是否幂等。决定超时后报 TIMEOUT（可安全重试）还是 TIMEOUT_UNKNOWN（结果未知）
    idempotent: bool



# =========================== 三、工具参数模型（第一道闸门） ===========================

# 所有工具参数模型的基类。模型（LLM）提交上来的是一个不可信的 JSON 字典，
# 必须先过这一层才能变成 handler 敢用的对象。
class StrictArgs(BaseModel):
    """模型只能提交 Schema 允许的业务候选参数。"""

    # extra="forbid"：多一个字段就报错，等价于 Go 的 decoder.DisallowUnknownFields()。
    # 这是防止模型注入 user_id / approved 之类越权字段的最后一道屏障，不能删。
    # strict=True：关闭隐式类型转换，字符串 "100" 不会被悄悄转成数字 100。
    model_config = ConfigDict(extra="forbid", strict=True)



# Field(pattern=...) 相当于 Go 结构体上的 validate:"regexp=..." 标签，运行时强制生效。
class GetOrderArgs(StrictArgs):
    order_id: str = Field(pattern=r"^ord_[0-9]{4}$")


class CreateRefundArgs(StrictArgs):
    order_id: str = Field(pattern=r"^ord_[0-9]{4}$")
    amount: float = Field(gt=0, le=10_000)
    reason: str = Field(min_length=4, max_length=200)


class RunShellArgs(StrictArgs):
    command: str = Field(min_length=1, max_length=200)



# 【作业·任务 2】转账参数。三条约束都在类型层完成，handler 里不必再校验一次：
#     账号形如 ACC-A-123456（银行前缀 + 单个大写字母 + 6 位数字）
#     金额必须大于 0 且不超过 100000（这是 Schema 上限，业务限额另见 transfer_precheck）
class TransferArgs(StrictArgs):
    from_account: str = Field(pattern=r"^ACC-[A-Z]-[0-9]{6}$")
    to_account: str = Field(pattern=r"^ACC-[A-Z]-[0-9]{6}$")
    amount: float = Field(gt=0, le=100_000)



# 下面四行是类型别名。| 是类型联合，Go 里没有直接对应物（最接近的是接口约束）。
# Handler / Precheck 都是函数类型：注意签名里没有 error 返回值，错误一律靠 raise 抛。
# CanonicalTarget 把参数压成一个字符串，供 deny/allow 规则做前缀匹配用。
ArgsModel = GetOrderArgs | CreateRefundArgs | RunShellArgs | TransferArgs
Handler = Callable[[str, ArgsModel, ExecutionContext], Awaitable[Mapping[str, Any]]]
Precheck = Callable[[ArgsModel, ExecutionContext], Awaitable[None]]
CanonicalTarget = Callable[[ArgsModel], str]


@dataclass(frozen=True, slots=True)

# =========================== 四、工具定义与框架内部结构 ===========================

# 一个工具的完整定义 = 给模型看的部分（name / description / 参数 schema）
#                   + 只给框架看的部分（policy / handler / precheck）。
# 两者在 to_model_tool 里被严格切开。
class ToolDefinition:
    name: str
    description: str
    parameters_model: type[StrictArgs]
    policy: ToolPolicy
    handler: Handler
    canonical_target: CanonicalTarget
    precheck: Precheck | None = None

    # 投影：这是模型唯一能看到的东西。handler、policy、precheck 一律不出现在返回值里，
    # 所以模型既不知道有审批这回事，也无法引用到真正执行的函数。
    def to_model_tool(self) -> dict[str, Any]:
        """只投影模型需要的描述和 JSON Schema，不暴露 handler 与治理策略。"""

        return {
            "type": "function",
            "function": {
                "name": self.name,
                "description": self.description,
                "parameters": self.parameters_model.model_json_schema(),
            },
        }


@dataclass(frozen=True, slots=True)

# 静态权限规则。target_prefix 为 None 表示匹配该工具的所有调用；
# 否则拿 canonical_target(args) 的前缀去比对（见 PermissionEngine._rule_matches）。
class PermissionRule:
    effect: Literal["allow", "deny"]
    tool_name: str
    target_prefix: str | None = None


@dataclass(frozen=True, slots=True)

# 一次权限判断的结论。code 是给上游程序判断的机器可读标识，reason 才是给人看的。
# source 记录"这个结论是哪一层下的"，排查线上问题时直接定位到 decide 的第几步。
class PermissionDecision:
    action: DecisionAction
    code: str
    reason: str
    source: Literal[
        "rule", "mode", "whitelist", "rbac", "business", "approval", "risk", "default"
    ]


@dataclass(frozen=True, slots=True)

# 模型发过来的一次原始调用。arguments 是不可信的字典，尚未经过任何校验。
class ToolCall:
    tool_call_id: str
    name: str
    arguments: Mapping[str, Any]


@dataclass(frozen=True, slots=True)

# 所有分支的统一出口结构：不论成功、拒绝、超时还是异常，都返回它，绝不往外抛异常。
# ok=False 且 action=CONFIRM 表示"挂起待确认"，不是失败。
class ToolResult:
    tool_call_id: str
    tool_name: str
    ok: bool
    action: DecisionAction
    code: str
    content: Any
    retryable: bool = False

    def to_tool_message(self) -> dict[str, Any]:
        return {
            "role": "tool",
            "tool_call_id": self.tool_call_id,
            "content": json.dumps(
                {
                    "ok": self.ok,
                    "code": self.code,
                    "action": self.action,
                    "content": self.content,
                },
                ensure_ascii=False,
            ),
        }


@dataclass(frozen=True, slots=True)

# 审计记录。注意只记 argument_keys（参数名），不记参数值——审计日志里不该出现明文账号。
# phase 区分两个阶段：decision（判断结果）与 execution（真的执行了，带耗时）。
class AuditRecord:
    trace_id: str
    tool_call_id: str
    tool_name: str
    user_id: str
    tenant_id: str
    phase: Literal["decision", "execution"]
    decision: str
    code: str
    argument_keys: tuple[str, ...]
    latency_ms: int | None = None


@dataclass(slots=True)

# 一条审批凭证。这里故意没有 frozen=True，因为 used 字段要在核销时被改写。
# digest 是参数摘要，审批与具体参数绑死，这是防"审批一笔小额、执行一笔大额"的关键。
class ApprovalRecord:
    approval_id: str
    user_id: str
    tenant_id: str
    tool_name: str
    digest: str
    expires_at: float
    used: bool = False



# 业务拒绝异常，带机器可读的 code（如 EXCEED_LIMIT）。
# Go 对照：自定义 error 类型 + errors.As 取出错误码；Python 这里靠 except 捕获再读 .code。
class PolicyDenied(RuntimeError):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code



# 瞬时故障异常（网络抖动一类）。只有只读或幂等的工具，框架才会自动重试它。
class TransientToolError(RuntimeError):
    pass


# =========================== 五、审批、审计与脱敏 ===========================

# 把任意参数递归压成"顺序稳定"的结构：字典按 key 排序、pydantic 对象转成字典。
# 目的是让同一份参数无论字段顺序如何，算出来的摘要都一样。
# Go 对照：类似先把 struct 转成 map 再按 key 排序序列化，避免 map 遍历顺序随机导致哈希不一致。
def _stable_value(value: Any) -> Any:
    if isinstance(value, BaseModel):
        return _stable_value(value.model_dump(mode="json"))
    if isinstance(value, Mapping):
        return {key: _stable_value(value[key]) for key in sorted(value)}
    if isinstance(value, (list, tuple)):
        return [_stable_value(item) for item in value]
    return value



# 审批摘要 = sha256(工具名 + 规范化后的完整参数)。
# 这是整个审批机制的地基：改任何一个参数，摘要就变，之前那张审批立刻失效。
def _approval_digest(tool_name: str, arguments: ArgsModel | Mapping[str, Any]) -> str:
    canonical = json.dumps(_stable_value(arguments), ensure_ascii=False, separators=(",", ":"))
    return hashlib.sha256(f"{tool_name}:{canonical}".encode()).hexdigest()



# 审批凭证的存放处。教学示例用内存字典，生产上应换成带 TTL 的 Redis 或数据库。
class ApprovalStore:
    def __init__(self) -> None:
        self._records: dict[str, ApprovalRecord] = {}

    # 签发一张审批。把"谁、哪个租户、哪个工具、哪份参数、有效到什么时候"一次性钉死。
    # 现实中这一步应该由人在 UI 上点"确认"触发，示例里由测试或 demo 直接调用。
    def approve(
        self,
        approval_id: str,
        context: ExecutionContext,
        tool_name: str,
        arguments: ArgsModel | Mapping[str, Any],
        *,
        # 有效期默认 5 分钟。注意参数列表里的 * ：它之后的参数只能用关键字传，防止调用方漏传搞错位置。
        ttl_seconds: float = 300,
    ) -> None:
        self._records[approval_id] = ApprovalRecord(
            approval_id=approval_id,
            user_id=context.user_id,
            tenant_id=context.tenant_id,
            tool_name=tool_name,
            digest=_approval_digest(tool_name, arguments),
            expires_at=time.time() + ttl_seconds,
        )


    # 核销一张审批。六个条件全部成立才算数，缺一不可：
    #     1. 凭证存在     2. 没被用过（一次性）   3. 没过期
    #     4. 同一个人     5. 同一个租户          6. 同一个工具 + 同一份参数（摘要相等）
    # 校验通过后立刻标记 used=True，所以同一张审批重放第二次必然失败。
    def consume(
        self,
        approval_id: str | None,
        context: ExecutionContext,
        tool_name: str,
        arguments: ArgsModel,
    ) -> bool:
        # approval_id 可能是 None；`approval_id or ""` 是 Python 惯用法，等价于 Go 里的空值兜底。
        record = self._records.get(approval_id or "")
        valid = bool(
            record
            and not record.used
            and record.expires_at >= time.time()
            and record.user_id == context.user_id
            and record.tenant_id == context.tenant_id
            and record.tool_name == tool_name
            and record.digest == _approval_digest(tool_name, arguments)
        )
        # 核销即作废。这一行是"一次性"三个字的全部实现。
        if valid and record:
            record.used = True
        return valid



# 审计落库口。教学示例只塞进内存列表，生产上写日志、写 Kafka 或写审计库。
# 关键不在于存到哪里，而在于 ToolRuntime 会在决策和执行两个阶段都主动写一条。
class AuditSink:
    def __init__(self) -> None:
        self.records: list[AuditRecord] = []

    def append(self, record: AuditRecord) -> None:
        self.records.append(record)



# 危险 Shell 命令的正则黑名单。正则只是教学兜底：真实环境必须靠窄接口、AST 解析和沙箱，
# 因为 `r''m -rf` 之类的变形随手就能绕过黑名单。
DANGEROUS_SHELL_PATTERNS = (
    re.compile(r"\brm\s+-rf\b", re.I),
    re.compile(r"\bgit\s+push\s+--force\b", re.I),
    re.compile(r"\bgit\s+reset\s+--hard\b", re.I),
    re.compile(r"\bsudo\b", re.I),
    re.compile(r"\bmkfs\b", re.I),
    re.compile(r">\s*/dev/", re.I),
)



# 【作业·任务 6】账号脱敏用的正则。(?P<name>...) 是命名捕获组，相当于 Go 的 (?P<name>...)，
# 用 match['prefix'] 取值。这里把 6 位数字拆成"前 2 位（丢弃）+ 后 4 位（保留）"。
ACCOUNT_PATTERN = re.compile(r"\b(?P<prefix>ACC-[A-Z]-)[0-9]{2}(?P<tail>[0-9]{4})\b")


def _is_dangerous_shell(command: str) -> bool:
    return any(pattern.search(command) for pattern in DANGEROUS_SHELL_PATTERNS)



# 结果脱敏。递归处理字典、列表和字符串三种情况：
#     1. 字典的 key 命中 token/secret/password/authorization 时，整个值替换成 ***
#     2. 字符串里的邮箱替换成 ***@***
#     3. 字符串里的账号替换成 ACC-A-****3456

# 位置很重要：它被 ToolRuntime 在 finalize 阶段统一调用，不在任何 handler 里。
# 这样新增工具的作者不可能忘记脱敏——这是"横切关注点上提"的典型例子。
def _redact(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {
            key: "***" if re.search(r"token|secret|password|authorization", key, re.I) else _redact(item)
            for key, item in value.items()
        }
    if isinstance(value, (list, tuple)):
        return [_redact(item) for item in value]
    if isinstance(value, str):
        masked = re.sub(r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}", "***@***", value)
        # 账号只保留银行前缀和后四位：ACC-A-123456 -> ACC-A-****3456
        return ACCOUNT_PATTERN.sub(lambda match: f"{match['prefix']}****{match['tail']}", masked)
    return value



# =========================== 六、权限状态机（本次作业禁止改动） ===========================

# 整个框架的核心。它只回答一个问题：这次调用能不能做。
# 至于怎么做，那是 handler 的事，两边通过 PermissionDecision 和 PolicyDenied 通信。

# decide 里九个步骤的顺序是固定框架，改顺序等于改安全模型。本文件的注释只增不改，
# 下面每一步的可执行代码与训练营原版完全一致。
class PermissionEngine:
    """固定优先级的三态权限状态机。"""

    def __init__(self, rules: Sequence[PermissionRule], approvals: ApprovalStore) -> None:
        self._rules = tuple(rules)
        self._approvals = approvals

    # 规则匹配：先比工具名，再比目标前缀。target_prefix 为 None 表示匹配该工具的全部调用。
    # canonical_target 由每个工具自己定义，例如 run_shell 用命令原文，transfer 用 转出->转入:金额。
    def _rule_matches(self, rule: PermissionRule, tool: ToolDefinition, arguments: ArgsModel) -> bool:
        if rule.tool_name != tool.name:
            return False
        if rule.target_prefix is None:
            return True
        return tool.canonical_target(arguments).startswith(rule.target_prefix)

    # 执行期授权。返回三态之一，绝不直接抛异常给调用方。

    # 九步的先后顺序是这节课的主要内容，读的时候对每一步问一句"能不能往上挪或往下挪"：
    #     1 deny 规则  -> 放最前，所以 bypass 模式也盖不掉硬拒绝
    #     2 plan 模式  -> 只读契约落在执行层，不是靠提示词约束模型
    #     3 工具白名单 -> 发现期过滤过了，执行期必须再查一次（模型可能报没给它的工具名）
    #     4 RBAC 权限  -> 只认 ExecutionContext，不认模型参数里自称的身份
    #     5 业务预检   -> 排在审批之前：余额不够就别去打扰人
    #     6 人工审批   -> 高风险写操作的闸门，与参数绑定且一次性
    #     7 bypass     -> 排在审批之后：它只能跳过普通确认，跳不过前面任何一道
    #     8 allow 规则 -> 永远最后生效，不能反超前面的硬边界
    #     9 危险命令   -> 正则兜底，仅对 Shell 类工具
    async def decide(
        self,
        tool: ToolDefinition,
        arguments: ArgsModel,
        context: ExecutionContext,
    ) -> PermissionDecision:
        # -- 第 1 步：deny 优先 ------------------------------------------------------
        # any(...) 里是生成器表达式，等价于 Go 的 for range + 提前 return true。
        # 拒绝永远优先于放行，这是所有权限系统的通用前提。
        # 1. deny-first：硬拒绝不能被 allow 或 bypass 覆盖。
        if any(
            rule.effect == "deny" and self._rule_matches(rule, tool, arguments)
            for rule in self._rules
        ):
            return PermissionDecision(DecisionAction.DENY, "DENY_RULE", "命中 deny 规则", "rule")

        # -- 第 2 步：plan 只读模式 --------------------------------------------------
        # `is not` 比较的是枚举身份（同一个对象），不是字符串内容，比 != 更严格也更快。
        # 把只读契约写在这里，模型就算被提示词注入骗过去了，执行层照样拦得住。
        # 2. plan 是执行层只读契约，而不是一句系统提示词。
        if context.mode is PermissionMode.PLAN and tool.policy.effect is not Effect.READ:
            return PermissionDecision(
                DecisionAction.DENY,
                "PLAN_MODE_DENIED",
                "plan 模式禁止写操作和 Shell",
                "mode",
            )

        # -- 第 3 步：执行期重查白名单 -----------------------------------------------
        # model_tools() 在发现期已经过滤过一遍了，这里为什么还要查？
        # 因为模型完全可以凭空编一个没给它的工具名调过来，发现期的过滤不是安全边界。
        # 3. 发现阶段过滤后，执行阶段仍然要重新检查白名单。
        if tool.name not in context.allowed_tools:
            return PermissionDecision(
                DecisionAction.DENY,
                "TOOL_NOT_ALLOWED",
                "工具不在本轮执行白名单",
                "whitelist",
            )

        # -- 第 4 步：RBAC 业务权限 --------------------------------------------------
        # 比对的是 context.permissions（认证层给的），不是参数里的任何字段。
        # 4. 只相信认证层生成的 ExecutionContext。
        if tool.policy.permission not in context.permissions:
            return PermissionDecision(
                DecisionAction.DENY,
                "PERMISSION_DENIED",
                f"缺少业务权限 {tool.policy.permission}",
                "rbac",
            )

        # -- 第 5 步：业务预检 -------------------------------------------------------
        # 调用工具自己的 precheck（转账的实现见 transfer_precheck）。
        # precheck 只允许判断、不允许改状态，它是副作用发生前的最后一次只读检查。
        # 它抛出的 PolicyDenied 在这里被接住，转成一个带业务错误码的 DENY。
        # 5. 资源归属、状态和额度在 handler 之前验证。
        try:
            if tool.precheck:
                await tool.precheck(arguments, context)
        except PolicyDenied as error:
            return PermissionDecision(DecisionAction.DENY, error.code, str(error), "business")

        # -- 第 6 步：人工审批 -------------------------------------------------------
        # 注意条件是 `requires_approval or risk is HIGH`，两者是或的关系：
        # 高风险工具的审批关不掉，单独把 requires_approval 改成 False 不起任何作用。

        # 三个分支的含义：
        #     审批核销成功       -> ALLOW，并且这张审批就此作废
        #     dontAsk 非交互模式 -> DENY，因为没有人能来点确认，挂起没有意义
        #     其余情况           -> CONFIRM，把球踢回给调用方去拿审批
        # 6. 高风险业务写操作必须使用一次性、参数绑定审批。
        if tool.policy.requires_approval or tool.policy.risk is Risk.HIGH:
            if self._approvals.consume(context.approval_id, context, tool.name, arguments):
                return PermissionDecision(
                    DecisionAction.ALLOW,
                    "APPROVED",
                    "审批与当前用户、租户、工具和参数完全匹配",
                    "approval",
                )
            if context.mode is PermissionMode.DONT_ASK:
                return PermissionDecision(
                    DecisionAction.DENY,
                    "APPROVAL_REQUIRED",
                    "非交互模式无法完成高风险确认",
                    "approval",
                )
            return PermissionDecision(
                DecisionAction.CONFIRM,
                "APPROVAL_REQUIRED",
                "需要确认本次具体动作",
                "approval",
            )

        # -- 第 7 步：bypass 模式 ----------------------------------------------------
        # 位置是重点：它在审批之后。所以 bypassPermissions 能省掉普通确认，
        # 但 deny 规则、plan、白名单、RBAC、预检、审批这六道它一道也绕不过。
        # 7. bypass 只能跳过普通确认，不能跳过前面的硬边界。
        if context.mode is PermissionMode.BYPASS_PERMISSIONS:
            return PermissionDecision(
                DecisionAction.ALLOW,
                "BYPASS_ALLOWED",
                "跳过普通确认，但硬边界已经全部通过",
                "mode",
            )

        # -- 第 8 步：allow 规则 -----------------------------------------------------
        # allow 是"免确认放行"，不是"提权"。它永远排在所有硬边界之后。
        # 8. allow 规则只在 deny、plan、白名单、RBAC 和审批以后生效。
        if any(
            rule.effect == "allow" and self._rule_matches(rule, tool, arguments)
            for rule in self._rules
        ):
            return PermissionDecision(DecisionAction.ALLOW, "ALLOW_RULE", "命中 allow 规则", "rule")

        # -- 第 9 步：危险命令兜底（仅 Shell） ---------------------------------------
        # getattr(arguments, "command", "") 是反射取字段，取不到就返回默认值 ""，
        # 因为只有 RunShellArgs 才有 command 字段。Go 里对应 reflect 或类型断言。
        # 9. 正则只是教学兜底，生产中必须配合窄工具、AST 与沙箱。
        if tool.policy.effect is Effect.SHELL and _is_dangerous_shell(
            str(getattr(arguments, "command", ""))
        ):
            if context.mode is PermissionMode.DONT_ASK:
                return PermissionDecision(
                    DecisionAction.DENY,
                    "DANGEROUS_OPERATION",
                    "危险 Shell 在非交互模式下被拒绝",
                    "risk",
                )
            return PermissionDecision(
                DecisionAction.CONFIRM,
                "DANGEROUS_OPERATION",
                "危险 Shell 需要用户确认",
                "risk",
            )

        # -- 兜底：九道检查全部通过，放行 --------------------------------------------
        return PermissionDecision(
            DecisionAction.ALLOW,
            "DEFAULT_ALLOWED",
            "所有确定性检查均已通过",
            "default",
        )



# =========================== 七、执行入口（整个框架的主干） ===========================

# 模型、CLI、测试都只能从这里调用工具。作业要求"测试不许绕过 runtime.invoke"，
# 就是因为一旦有人直接调 handler，上面所有治理层就全部形同虚设。
class ToolRuntime:
    """模型、CLI、测试与未来 Provider 共用的唯一工具执行入口。"""

    def __init__(
        self,
        tools: Sequence[ToolDefinition],
        permission_engine: PermissionEngine,
        audit_sink: AuditSink,
    ) -> None:
        # 字典推导式，等价于 Go 里 for range 往 map[string]*ToolDefinition 里塞。
        self._tools = {tool.name: tool for tool in tools}
        self._permission_engine = permission_engine
        self._audit = audit_sink

    # 发现期：告诉模型"你这轮有哪些工具可用"。
    # 两层收窄：先按白名单过滤工具，再用 to_model_tool 把 handler 和 policy 摘掉。
    # 注意这只是减少误用，不是安全边界——真正的边界在 invoke 里（见 decide 第 3 步）。
    def model_tools(self, context: ExecutionContext) -> list[dict[str, Any]]:
        """发现期白名单：减少模型可见能力，不把 handler 暴露给模型。"""

        return [
            tool.to_model_tool()
            for tool in self._tools.values()
            if tool.name in context.allowed_tools
        ]

    # 一次工具调用的完整生命周期，四个阶段依次推进，顺序不可交换：

    #     prepare-1  参数校验：把不可信字典变成业务对象
    #     prepare-2  执行期授权：三态决策 + 写决策审计
    #     execute    真正执行：超时、重试、异常都在这里收敛
    #     finalize   收尾：脱敏 -> 写执行审计 -> 返回模型可见结果

    # 重要约定：这个方法永远不往外抛异常，所有失败都变成一个 ToolResult 返回。
    # 因为调用方是模型，它需要的是一个能读懂的错误码，而不是一段 traceback。
    async def invoke(self, call: ToolCall, context: ExecutionContext) -> ToolResult:
        # perf_counter 是单调时钟，只能用来测耗时，不能当墙上时间（对应 Go 的 time.Since）。
        started = time.perf_counter()
        tool = self._tools.get(call.name)
        # 模型报了一个根本不存在的工具名——这在真实链路里很常见，直接拒绝并留痕。
        if tool is None:
            return self._rejected(call, context, "TOOL_NOT_FOUND", "工具不存在")

        # ---------- 阶段一：参数校验 ----------
        # model_validate 相当于 json.Unmarshal + 全部校验规则一次跑完。
        # 过了这一关，后面所有代码拿到的都是合法对象，不必再写一遍防御性判断。
        # prepare-1：Pydantic 把不可信字典转换成 handler 可接收的业务对象。
        try:
            arguments = tool.parameters_model.model_validate(call.arguments)
        except ValidationError as error:
            # 把 pydantic 的报错整理成 [{path, message}]，让模型知道该改哪个字段、怎么改。
            # 只给结构化信息，不把库的原始异常文本直接丢给模型。
            details = [
                {"path": ".".join(map(str, item["loc"])), "message": item["msg"]}
                for item in error.errors(include_url=False)
            ]
            return self._rejected(call, context, "INVALID_ARGUMENT", details)

        # ---------- 阶段二：执行期授权 ----------
        # 无论决策是什么，都先写一条 decision 审计——被拒绝的调用同样要留痕。
        # prepare-2：执行期重新授权，返回 allow / deny / confirm。
        decision = await self._permission_engine.decide(tool, arguments, context)
        self._audit.append(
            AuditRecord(
                trace_id=context.trace_id,
                tool_call_id=call.tool_call_id,
                tool_name=call.name,
                user_id=context.user_id,
                tenant_id=context.tenant_id,
                phase="decision",
                decision=decision.action,
                code=decision.code,
                # 只记参数名并排序，不记参数值。审计日志里不该出现明文账号和金额。
                argument_keys=tuple(sorted(call.arguments)),
            )
        )
        # DENY 和 CONFIRM 都在这里返回。注意到此为止，handler 一次都没有被碰过。
        if decision.action is not DecisionAction.ALLOW:
            return ToolResult(
                tool_call_id=call.tool_call_id,
                tool_name=call.name,
                ok=False,
                action=decision.action,
                code=decision.code,
                content=decision.reason,
            )

        # ---------- 阶段三：执行 ----------
        # 三类异常分别映射成不同的错误码，绝不把 traceback 透给模型：
        # execute：只有通过全部确定性检查后，handler 才可能产生副作用。
        try:
            raw = await self._execute_with_recovery(tool, call.tool_call_id, arguments, context)
        except TimeoutError:
            # 超时的语义由幂等性决定，这是这一段最值得琢磨的一行：
            #     只读或幂等   -> TIMEOUT，调用方可以安全重试
            #     非幂等写操作 -> TIMEOUT_UNKNOWN，框架无权断言副作用有没有发生，只能承认结果未知
            # 转账正是后者，所以它超时后返回的是 TIMEOUT_UNKNOWN。
            code = "TIMEOUT" if tool.policy.effect is Effect.READ or tool.policy.idempotent else "TIMEOUT_UNKNOWN"
            return self._failed(call, context, started, code, "工具执行超时")
        # handler 内部抛出的业务拒绝，例如转入账户不存在（ACCOUNT_NOT_FOUND）。
        except PolicyDenied as error:
            return self._failed(call, context, started, error.code, str(error))
        except Exception as error:  # 生产中映射异常类型，不把 traceback 交给模型。
            return self._failed(call, context, started, "TOOL_ERROR", str(error))

        # ---------- 阶段四：脱敏与审计 ----------
        # handler 返回的是明文数据（账号、邮箱、token 都在里面），
        # 在这里统一过一遍 _redact 之后，才允许进入模型上下文。
        # finalize：先投影与脱敏，再形成模型能看见的 ToolResult。
        safe_content = _redact(dict(raw))
        latency_ms = round((time.perf_counter() - started) * 1_000)
        self._audit.append(
            AuditRecord(
                trace_id=context.trace_id,
                tool_call_id=call.tool_call_id,
                tool_name=call.name,
                user_id=context.user_id,
                tenant_id=context.tenant_id,
                phase="execution",
                decision="executed",
                code="OK",
                argument_keys=tuple(sorted(call.arguments)),
                latency_ms=latency_ms,
            )
        )
        return ToolResult(call.tool_call_id, call.name, True, DecisionAction.ALLOW, "OK", safe_content)


    # 带重试和超时的执行包装。两条策略：
    #     1. 只有只读或幂等的工具才允许重试，非幂等写操作一律 retries=0（转账属于这一类）
    #     2. 每次尝试都套一层超时，超时会让 handler 里的 await 直接被取消

    # asyncio.timeout 是异步上下文管理器，作用类似 Go 的 context.WithTimeout，
    # 区别是取消靠抛异常实现：超时瞬间，handler 停在它当时那个 await 上，后面的代码不再执行。
    async def _execute_with_recovery(
        self,
        tool: ToolDefinition,
        tool_call_id: str,
        arguments: ArgsModel,
        context: ExecutionContext,
    ) -> Mapping[str, Any]:
        retries = tool.policy.max_retries if tool.policy.effect is Effect.READ or tool.policy.idempotent else 0
        for attempt in range(retries + 1):
            try:
                async with asyncio.timeout(tool.policy.timeout_seconds):
                    return await tool.handler(tool_call_id, arguments, context)
            except TransientToolError:
                if attempt == retries:
                    raise
                # 指数退避，最多等 0.2 秒。
                await asyncio.sleep(min(0.05 * (2**attempt), 0.2))
        # 理论上走不到：循环要么 return 要么 raise。留着是为了让类型检查器闭嘴，也防御未来改坏。
        raise AssertionError("unreachable")


    # 决策阶段的拒绝出口：写一条 decision 审计，再返回统一结构的 ToolResult。
    def _rejected(
        self,
        call: ToolCall,
        context: ExecutionContext,
        code: str,
        content: Any,
    ) -> ToolResult:
        self._audit.append(
            AuditRecord(
                trace_id=context.trace_id,
                tool_call_id=call.tool_call_id,
                tool_name=call.name,
                user_id=context.user_id,
                tenant_id=context.tenant_id,
                phase="decision",
                decision="deny",
                code=code,
                argument_keys=tuple(sorted(call.arguments)),
            )
        )
        return ToolResult(call.tool_call_id, call.name, False, DecisionAction.DENY, code, content)


    # 执行阶段的失败出口：写一条 execution 审计（带耗时），再返回统一结构的 ToolResult。
    # 与 _rejected 的区别在于 phase 不同——一个是没让做，一个是做了但没成。
    def _failed(
        self,
        call: ToolCall,
        context: ExecutionContext,
        started: float,
        code: str,
        content: Any,
    ) -> ToolResult:
        self._audit.append(
            AuditRecord(
                trace_id=context.trace_id,
                tool_call_id=call.tool_call_id,
                tool_name=call.name,
                user_id=context.user_id,
                tenant_id=context.tenant_id,
                phase="execution",
                decision="failed",
                code=code,
                argument_keys=tuple(sorted(call.arguments)),
                latency_ms=round((time.perf_counter() - started) * 1_000),
            )
        )
        return ToolResult(call.tool_call_id, call.name, False, DecisionAction.DENY, code, content)


# =========================== 八、模拟数据与工具实现 ===========================

# 教学用内存数据。key 是 (租户, 订单号) 组成的元组——Python 的元组可以直接做字典键，
# 相当于 Go 里用一个可比较的 struct 当 map key。这样每次查询都被迫带上租户，天然隔离。
ORDERS = {
    ("tenant_a", "ord_1001"): {
        "status": "paid",
        "refundable": 399.0,
        "customer_email": "alice@example.com",
    }
}

# 【作业·任务 1】模拟账户余额，key 同样是 (租户, 账号)。
# 注意 tenant_b 的账户对 tenant_a 来说等于不存在——跨租户转账会直接落到余额不足分支。
ACCOUNTS: dict[tuple[str, str], float] = {
    ("tenant_a", "ACC-A-123456"): 100_000.0,
    ("tenant_a", "ACC-A-654321"): 5_000.0,
    ("tenant_a", "ACC-A-888888"): 20_000.0,
    ("tenant_b", "ACC-B-111111"): 50_000.0,
}
# 初始余额快照。dict(ACCOUNTS) 是浅拷贝，用于测试之间复位账本。
INITIAL_ACCOUNTS: Mapping[tuple[str, str], float] = dict(ACCOUNTS)
# 单笔转账限额。抽成模块级常量而不是写死在函数里，测试才能用 monkeypatch 临时放开它，
# 去验证被限额挡住的超时分支（见 test_transfer_timeout_reports_unknown_without_side_effects）。
# 数字里的下划线只是可读性分隔符，50_000.0 就是 50000.0。
SINGLE_TRANSFER_LIMIT = 50_000.0
# 副作用计数器。测试靠它断言"被拒绝的调用真的一次都没执行"。
SIDE_EFFECTS = {"refund_executions": 0, "shell_executions": 0, "transfer_executions": 0}



# 复位账本。注意是 clear + update 而不是重新赋值——因为别处已经 import 了 ACCOUNTS 这个对象，
# 直接 ACCOUNTS = {...} 只会换掉本模块的名字绑定，别人手里还是旧字典。这是 Python 的常见坑。
def reset_accounts() -> None:
    ACCOUNTS.clear()
    ACCOUNTS.update(INITIAL_ACCOUNTS)


def reset_side_effects() -> None:
    SIDE_EFFECTS.update(refund_executions=0, shell_executions=0, transfer_executions=0)
    reset_accounts()



# --------------------------- 工具实现：查询订单（只读） ---------------------------
# 函数名前缀下划线的参数（_tool_call_id）表示"签名需要但用不上"，是 Python 的命名约定。
# 返回值里故意塞了一个 access_token，用来演示 _redact 会把它换成 ***。
async def get_order_handler(
    _tool_call_id: str,
    raw_arguments: ArgsModel,
    context: ExecutionContext,
) -> Mapping[str, Any]:
    arguments = raw_arguments
    # 运行时窄化类型。因为 Handler 的签名接收的是联合类型 ArgsModel，
    # 这一行既给类型检查器一个凭据，也在真的传错模型时立刻炸掉。相当于 Go 的类型断言。
    assert isinstance(arguments, GetOrderArgs)
    order = ORDERS.get((context.tenant_id, arguments.order_id))
    if not order:
        raise PolicyDenied("ORDER_NOT_FOUND", "当前租户下不存在该订单")
    return {**order, "access_token": "tok_demo_should_not_leak"}



# --------------------------- 工具实现：退款（高风险写） ---------------------------
# 预检的写法范本：查订单归属和状态、比对可退金额，不满足就抛 PolicyDenied。
# 它在 decide 第 5 步被调用，全程只读。
async def refund_precheck(raw_arguments: ArgsModel, context: ExecutionContext) -> None:
    arguments = raw_arguments
    assert isinstance(arguments, CreateRefundArgs)
    order = ORDERS.get((context.tenant_id, arguments.order_id))
    if not order or order["status"] != "paid":
        raise PolicyDenied("BUSINESS_RULE_DENIED", "订单不存在或状态不可退款")
    if arguments.amount > float(order["refundable"]):
        raise PolicyDenied("BUSINESS_RULE_DENIED", "退款金额超过可退金额")



# 退款 handler。真实实现里这里会调支付网关，所以 tool_call_id 被当成幂等键传下去。
async def create_refund_handler(
    tool_call_id: str,
    raw_arguments: ArgsModel,
    context: ExecutionContext,
) -> Mapping[str, Any]:
    arguments = raw_arguments
    assert isinstance(arguments, CreateRefundArgs)
    SIDE_EFFECTS["refund_executions"] += 1
    return {
        "refund_id": "ref_9001",
        "idempotency_key": tool_call_id,
        "tenant_id": context.tenant_id,
        "order_id": arguments.order_id,
        "amount": arguments.amount,
        "status": "accepted",
    }



# --------------------------- 工具实现：模拟 Shell ---------------------------
# 教学用，不创建真实子进程。真实环境必须放进沙箱，并且优先用窄接口替代裸命令。
async def simulated_shell_handler(
    _tool_call_id: str,
    raw_arguments: ArgsModel,
    _context: ExecutionContext,
) -> Mapping[str, Any]:
    arguments = raw_arguments
    assert isinstance(arguments, RunShellArgs)
    SIDE_EFFECTS["shell_executions"] += 1
    return {
        "simulated": True,
        "command": arguments.command,
        "stdout": "教学模拟：没有创建真实子进程",
    }



# --------------------------- 工具实现：转账（本次作业） ---------------------------

# 【作业·任务 3】业务预检。三条纪律：
#     1. 只判断，不改任何状态——它在 decide 第 5 步跑，此时还没到执行阶段
#     2. 有先后顺序：先限额后余额，因为限额是政策问题，余额是资金问题
#     3. 每个拒绝都带机器可读的错误码，模型看到 EXCEED_LIMIT 会改金额，
#        看到 INSUFFICIENT_BALANCE 会换账户——给一句中文描述它只能瞎猜
async def transfer_precheck(raw_arguments: ArgsModel, context: ExecutionContext) -> None:
    arguments = raw_arguments
    assert isinstance(arguments, TransferArgs)
    # 1. 单笔限额：额度是业务边界，必须在 handler 之前拦截。
    if arguments.amount > SINGLE_TRANSFER_LIMIT:
        raise PolicyDenied("EXCEED_LIMIT", "单笔转账金额超过限额")
    # 2. 余额充足：只读校验，不改账本。
    # 查余额必须带 context.tenant_id：拿 A 租户的身份查 B 租户的账号，这里会返回 None。
    # 这就是跨租户转账被挡住的地方，不需要额外写一条租户校验。
    balance = ACCOUNTS.get((context.tenant_id, arguments.from_account))
    if balance is None or balance < arguments.amount:
        raise PolicyDenied("INSUFFICIENT_BALANCE", "转出账户余额不足")



# 【作业·任务 4】转账执行。这是整个文件里唯一允许产生副作用的地方。

# 三处顺序都是有意安排的，改了顺序就会出事（阅读指南里有对应的破坏性实验）：
#     1. 超时模拟排在最前：超时发生时账本还没动，所以钱是安全的
#     2. 转入账户检查排在扣款之前：否则会出现"钱扣了但没人收"
#     3. 计数器放在两笔账都改完之后：保证它和真实副作用一致
async def transfer_handler(
    tool_call_id: str,
    raw_arguments: ArgsModel,
    context: ExecutionContext,
) -> Mapping[str, Any]:
    arguments = raw_arguments
    assert isinstance(arguments, TransferArgs)
    # 教学用超时模拟：睡 3 秒，而工具策略里的超时是 2 秒，必然超时。
    # 注意这里被 asyncio.timeout 取消后，下面的代码一行都不会执行。
    if arguments.amount > 80_000:
        await asyncio.sleep(3.0)

    # 两个 key 都带租户，和 ACCOUNTS 的结构一致。
    from_key = (context.tenant_id, arguments.from_account)
    to_key = (context.tenant_id, arguments.to_account)
    # 转入账户不存在（含跨租户的情况）。此时一分钱都还没动，直接抛业务拒绝，
    # 异常会被 invoke 的 except PolicyDenied 接住，变成 ACCOUNT_NOT_FOUND。
    if to_key not in ACCOUNTS:
        raise PolicyDenied("ACCOUNT_NOT_FOUND", "转入账户不存在")

    # 真正的副作用：一扣一加。
    # 教学示例是单线程 asyncio，所以不用加锁；真实系统这两行必须在一个数据库事务里。
    ACCOUNTS[from_key] -= arguments.amount
    ACCOUNTS[to_key] += arguments.amount
    SIDE_EFFECTS["transfer_executions"] += 1
    return {
        # 用 tool_call_id 后 6 位当流水号。f"" 是格式化字符串，[-6:] 是切片取末尾 6 个字符。
        # 返回的是明文账号，脱敏由 runtime 统一负责，handler 不操心。
        "txn_id": f"txn_{tool_call_id[-6:]}",
        "from": arguments.from_account,
        "to": arguments.to_account,
        "amount": arguments.amount,
        "status": "succeeded",
    }



# =========================== 九、工具注册与运行时组装 ===========================

# 把工具接进框架的唯一入口。ToolPolicy 的七个位置参数依次是：
#     effect, risk, permission, requires_approval, timeout_seconds, max_retries, idempotent
def build_tools() -> list[ToolDefinition]:
    return [
        ToolDefinition(
            name="get_order",
            description="查询当前租户订单状态和可退金额",
            parameters_model=GetOrderArgs,
            # 只读 + 幂等，所以允许重试 2 次，超时后报的是可安全重试的 TIMEOUT。
            policy=ToolPolicy(Effect.READ, Risk.MEDIUM, "order:read", False, 1.0, 2, True),
            handler=get_order_handler,
            canonical_target=lambda args: str(getattr(args, "order_id")),
        ),
        ToolDefinition(
            name="create_refund",
            description="为当前租户的已支付订单创建退款",
            parameters_model=CreateRefundArgs,
            policy=ToolPolicy(Effect.WRITE, Risk.HIGH, "refund:create", True, 2.0, 0, False),
            handler=create_refund_handler,
            precheck=refund_precheck,
            canonical_target=lambda args: f"{getattr(args, 'order_id')}:{getattr(args, 'amount')}",
        ),
        ToolDefinition(
            name="run_shell",
            description="教学用模拟 Shell，不执行真实系统命令",
            parameters_model=RunShellArgs,
            policy=ToolPolicy(Effect.SHELL, Risk.MEDIUM, "shell:run", False, 1.0, 0, False),
            handler=simulated_shell_handler,
            canonical_target=lambda args: str(getattr(args, "command")),
        ),
        ToolDefinition(
            name="transfer",
            description="在当前租户的账户之间发起一笔转账",
            parameters_model=TransferArgs,
            # 【作业·任务 5】转账的治理画像，逐个字段看：
            #     Effect.WRITE  写操作，plan 模式下直接拒绝
            #     Risk.HIGH     高风险，自动触发审批（这一项就足够了，不依赖下面的 True）
            #     True          显式要求审批，写出来是为了表达意图
            #     2.0           超时 2 秒，正好小于 handler 里模拟的 3 秒
            #     0             不重试：非幂等的转账重试一次就是重复扣款
            #     False         非幂等，所以超时报 TIMEOUT_UNKNOWN 而不是 TIMEOUT
            policy=ToolPolicy(Effect.WRITE, Risk.HIGH, "transfer:execute", True, 2.0, 0, False),
            handler=transfer_handler,
            precheck=transfer_precheck,
            # 规范化目标：转出->转入:金额。lambda 是匿名函数，供 deny/allow 规则做前缀匹配。
            # 例如可以加一条规则禁止向某个账号前缀转账。
            canonical_target=lambda args: (
                f"{getattr(args, 'from_account')}->{getattr(args, 'to_account')}:{getattr(args, 'amount')}"
            ),
        ),
    ]



# 静态权限规则表。前两条是 deny（decide 第 1 步命中，任何模式都盖不掉），
# 第三条 allow 让 pytest 免确认放行（decide 第 8 步才生效，排在所有硬边界之后）。
DEFAULT_RULES = (
    PermissionRule("deny", "run_shell", "rm -rf"),
    PermissionRule("deny", "run_shell", "git push --force"),
    PermissionRule("allow", "run_shell", "pytest"),
)



# 构造一个演示/测试用的执行上下文。**overrides 是关键字参数收集，
# 配合 dataclasses.replace 就能写成 base_context(mode=PermissionMode.PLAN) 这种局部覆盖。
# 真实系统里这个对象必须由认证层生成，绝不能让调用方自己拼。
def base_context(**overrides: Any) -> ExecutionContext:
    context = ExecutionContext(
        trace_id="trace_demo",
        user_id="u_100",
        tenant_id="tenant_a",
        mode=PermissionMode.DEFAULT,
        # 作业里新增了 transfer:execute 权限和 transfer 工具白名单，否则转账会停在 decide 第 3、4 步。
        permissions=frozenset({"order:read", "refund:create", "shell:run", "transfer:execute"}),
        allowed_tools=frozenset({"get_order", "create_refund", "run_shell", "transfer"}),
    )
    # 因为 ExecutionContext 是 frozen 的，改字段只能复制一份新的（Go 里直接改字段的值拷贝）。
    return replace(context, **overrides)



# 组装运行时：规则表 + 审批库 -> 权限引擎 -> 加上工具列表和审计口 -> ToolRuntime。
# 签名里的 * 表示后面三个只能用关键字传参。测试靠它注入自己的 ApprovalStore 和 AuditSink。
def build_runtime(
    *,
    approvals: ApprovalStore | None = None,
    audit: AuditSink | None = None,
    rules: Sequence[PermissionRule] = DEFAULT_RULES,
) -> tuple[ToolRuntime, ApprovalStore, AuditSink]:
    approval_store = approvals or ApprovalStore()
    audit_sink = audit or AuditSink()
    engine = PermissionEngine(rules, approval_store)
    return ToolRuntime(build_tools(), engine, audit_sink), approval_store, audit_sink



# =========================== 十、离线演示与真实 Agent Loop ===========================

# 不连大模型的完整演示，九次调用覆盖了治理链路的主要分支：
#     call_01 只读放行            call_02 高风险缺审批 -> confirm
#     call_03 带审批执行成功      call_04 模型注入越权字段 -> INVALID_ARGUMENT
#     call_05 危险命令命中 deny   call_06 plan 模式拒绝写操作
#     call_07 转账缺审批 -> confirm
#     call_08 转账带审批成功（返回的账号已脱敏）
#     call_09 同一张审批换了金额 -> 先被业务预检的 EXCEED_LIMIT 挡住
async def run_offline_demo() -> None:
    reset_side_effects()
    runtime, approvals, audit = build_runtime()
    context = base_context()
    refund_arguments = {"order_id": "ord_1001", "amount": 399.0, "reason": "商品存在质量问题"}

    results = [
        await runtime.invoke(ToolCall("call_01", "get_order", {"order_id": "ord_1001"}), context),
        await runtime.invoke(ToolCall("call_02", "create_refund", refund_arguments), context),
    ]
    approvals.approve("approval_01", context, "create_refund", refund_arguments)
    results.append(
        await runtime.invoke(
            ToolCall("call_03", "create_refund", refund_arguments),
            replace(context, approval_id="approval_01"),
        )
    )
    results.extend(
        [
            await runtime.invoke(
                ToolCall(
                    "call_04",
                    "create_refund",
                    {**refund_arguments, "user_id": "admin", "approved": True},
                ),
                context,
            ),
            await runtime.invoke(
                ToolCall("call_05", "run_shell", {"command": "rm -rf /tmp/demo"}),
                replace(context, mode=PermissionMode.BYPASS_PERMISSIONS),
            ),
            await runtime.invoke(
                ToolCall("call_06", "create_refund", refund_arguments),
                replace(context, mode=PermissionMode.PLAN, approval_id="approval_01"),
            ),
        ]
    )

    transfer_arguments = {
        "from_account": "ACC-A-123456",
        "to_account": "ACC-A-654321",
        "amount": 1_200.0,
    }
    results.append(await runtime.invoke(ToolCall("call_07", "transfer", transfer_arguments), context))
    approvals.approve("approval_02", context, "transfer", transfer_arguments)
    results.append(
        await runtime.invoke(
            ToolCall("call_08", "transfer", transfer_arguments),
            replace(context, approval_id="approval_02"),
        )
    )
    results.append(
        await runtime.invoke(
            ToolCall("call_09", "transfer", {**transfer_arguments, "amount": 60_000.0}),
            replace(context, approval_id="approval_02"),
        )
    )

    for result in results:
        print(json.dumps(result.__dict__ if hasattr(result, "__dict__") else {
            "tool_call_id": result.tool_call_id,
            "tool_name": result.tool_name,
            "ok": result.ok,
            "action": result.action,
            "code": result.code,
            "content": result.content,
        }, ensure_ascii=False, default=str))
    print(json.dumps({"side_effects": SIDE_EFFECTS, "audit_records": len(audit.records)}, ensure_ascii=False))
    print(json.dumps({"balances": {f"{key[0]}/{_redact(key[1])}": value for key, value in ACCOUNTS.items()}}, ensure_ascii=False))
    for record in audit.records:
        print(json.dumps(record.__dict__ if hasattr(record, "__dict__") else {
            "trace_id": record.trace_id,
            "tool_call_id": record.tool_call_id,
            "tool_name": record.tool_name,
            "user_id": record.user_id,
            "tenant_id": record.tenant_id,
            "phase": record.phase,
            "decision": record.decision,
            "code": record.code,
            "argument_keys": record.argument_keys,
            "latency_ms": record.latency_ms,
        }, ensure_ascii=False, default=str))



# 可选的真实模型闭环。重点只有一个：模型吐出来的 tool_calls 依然逐个走 runtime.invoke，
# 治理链路一步都不少。模型只是决定"调哪个工具、传什么参数"，它无权决定能不能执行。
async def run_deepseek_agent(user_input: str) -> None:
    """可选真实模型闭环；所有工具调用仍经过同一个 ToolRuntime.invoke。"""

    from openai import AsyncOpenAI

    api_key = os.environ.get("DEEPSEEK_API_KEY")
    if not api_key:
        raise RuntimeError("请先设置环境变量 DEEPSEEK_API_KEY")

    runtime, _, _ = build_runtime()
    # 刻意只放开只读工具：真实模型闭环的演示里不该让它碰到退款和转账。
    context = base_context(allowed_tools=frozenset({"get_order"}))
    client = AsyncOpenAI(api_key=api_key, base_url=os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com"))
    model = os.getenv("DEEPSEEK_MODEL", "deepseek-v4-flash")
    messages: list[dict[str, Any]] = [
        {
            "role": "system",
            "content": "你是订单助手。只根据工具结果回答，不得伪造订单事实。",
        },
        {"role": "user", "content": user_input},
    ]

    for _round in range(8):
        stream = await client.chat.completions.create(
            model=model,
            messages=messages,
            tools=runtime.model_tools(context),
            stream=True,
            extra_body={"thinking": {"type": "disabled"}},
        )
        text_parts: list[str] = []
        pending_calls: dict[int, dict[str, Any]] = {}

        async for chunk in stream:
            if not chunk.choices:
                continue
            delta = chunk.choices[0].delta
            if delta.content:
                text_parts.append(delta.content)
                print(delta.content, end="", flush=True)
            for delta_call in delta.tool_calls or []:
                current = pending_calls.setdefault(
                    delta_call.index,
                    {"id": "", "type": "function", "function": {"name": "", "arguments": ""}},
                )
                if delta_call.id:
                    current["id"] = delta_call.id
                if delta_call.function:
                    if delta_call.function.name:
                        current["function"]["name"] += delta_call.function.name
                    if delta_call.function.arguments:
                        current["function"]["arguments"] += delta_call.function.arguments

        provider_calls = [pending_calls[index] for index in sorted(pending_calls)]
        assistant_message: dict[str, Any] = {"role": "assistant", "content": "".join(text_parts)}
        if provider_calls:
            assistant_message["tool_calls"] = provider_calls
        messages.append(assistant_message)

        if not provider_calls:
            print()
            return

        if text_parts:
            print()
        for provider_call in provider_calls:
            try:
                raw_arguments = json.loads(provider_call["function"]["arguments"])
            except json.JSONDecodeError:
                raw_arguments = {"_invalid_json": provider_call["function"]["arguments"]}
            # 模型说要调工具，也必须从同一个入口进来，和测试、CLI 走的是同一条主干。
            result = await runtime.invoke(
                ToolCall(provider_call["id"], provider_call["function"]["name"], raw_arguments),
                context,
            )
            print(f"[tool_result] {result.tool_name} {result.code}")
            messages.append(result.to_tool_message())

    raise RuntimeError("Agent Loop 超过最大轮数 8")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Python 工具治理与权限状态机演示")
    parser.add_argument("--agent", action="store_true", help="使用 DeepSeek 运行真实 Agent Loop")
    parser.add_argument("--input", default="请查询订单 ord_1001 的状态和可退金额")
    return parser.parse_args()


if __name__ == "__main__":
    cli_args = parse_args()
    asyncio.run(run_deepseek_agent(cli_args.input) if cli_args.agent else run_offline_demo())