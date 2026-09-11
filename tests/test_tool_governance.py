# ============================================================================
# pytest 速查（Go 背景读者）：
#
#   pytest 概念                     Go 里大致对应什么
#   -----------------------------   ----------------------------------------
#   test_xxx 函数                   func TestXxx(t *testing.T)，靠函数名前缀发现
#   assert x == y                   if x != y { t.Fatalf(...) }，失败信息由框架生成
#   @pytest.fixture(autouse=True)   每个用例自动跑的 setup/teardown，yield 前后分界
#   monkeypatch                     临时替换包级变量/函数，用例结束自动还原
#   -k transfer                     只跑名字里含 transfer 的用例，类似 -run 'Transfer'
#
# 这里所有用例都用 asyncio.run 包一层去调 async 函数，
# 是为了不依赖 pytest-asyncio 插件，装了 pytest 就能跑。
# ============================================================================
"""第二章作业：转账工具的治理链路测试。

所有调用都必须经过 ToolRuntime.invoke，不允许直接调用 transfer_handler。
"""

from __future__ import annotations

import asyncio
from dataclasses import replace
from typing import Any

import pytest

import tool_governance_demo as demo
from tool_governance_demo import (
    ACCOUNTS,
    DecisionAction,
    ToolCall,
    base_context,
    build_runtime,
    reset_side_effects,
)

FROM_ACCOUNT = "ACC-A-123456"
TO_ACCOUNT = "ACC-A-654321"
SMALL_ACCOUNT = "ACC-A-888888"


# autouse=True 表示每个用例都自动生效，不需要在参数里声明。
# yield 之前是 setup，之后是 teardown：把账本和副作用计数器复位，
# 保证用例之间互不污染（上一个用例扣掉的钱不会影响下一个）。
@pytest.fixture(autouse=True)
def _clean_state() -> None:
    reset_side_effects()
    yield
    reset_side_effects()



# 统一的调用入口包装。作业要求测试不许绕过 runtime.invoke 直接调 handler，
# 否则参数校验、权限、审批、脱敏、审计这一整条链路就都被跳过了。
# asyncio.run 每次新建一个事件循环跑完这个协程，相当于同步地等一个 goroutine 结束。
def invoke(runtime, call: ToolCall, context) -> Any:
    return asyncio.run(runtime.invoke(call, context))



# 构造一份合法的转账参数，**overrides 用来局部覆盖某个字段。
# 注意这里返回的是原始字典（模型发过来的样子），不是 TransferArgs 对象——
# 校验是 runtime 的职责，测试要模拟的是"未经校验的输入"。
def transfer_args(**overrides: Any) -> dict[str, Any]:
    return {"from_account": FROM_ACCOUNT, "to_account": TO_ACCOUNT, "amount": 1_000.0, **overrides}



# 拷一份当前账本快照，用来断言"余额一分没动"。
def balances() -> dict[tuple[str, str], float]:
    return dict(ACCOUNTS)


def test_transfer_rejects_invalid_arguments() -> None:
    """任务 2：Schema 是第一道闸门，格式、范围与额外字段都进不了 handler。"""

    runtime, _, _ = build_runtime()
    context = base_context()
    before = balances()

    # 五种非法输入，覆盖格式、边界和注入三类问题：
    #     小写字母、位数不足   -> 正则不匹配
    #     金额 0、超过上限     -> Field(gt=0, le=100_000) 不满足
    #     多传 approved 字段   -> extra="forbid" 拦截，这条是防模型越权注入的关键
    bad_calls = {
        "bad_format": transfer_args(from_account="ACC-a-123456"),
        "short_digits": transfer_args(to_account="ACC-A-12345"),
        "non_positive": transfer_args(amount=0.0),
        "over_schema_max": transfer_args(amount=100_000.01),
        "extra_field": {**transfer_args(), "approved": True},
    }

    for name, arguments in bad_calls.items():
        result = invoke(runtime, ToolCall(f"call_{name}", "transfer", arguments), context)
        assert result.ok is False, name
        assert result.action is DecisionAction.DENY, name
        assert result.code == "INVALID_ARGUMENT", name

    # extra="forbid" 必须明确报出多余字段，而不是被静默忽略。
    extra_result = invoke(
        runtime, ToolCall("call_extra", "transfer", {**transfer_args(), "approved": True}), context
    )
    assert any(item["path"] == "approved" for item in extra_result.content)

    assert balances() == before
    assert demo.SIDE_EFFECTS["transfer_executions"] == 0


def test_transfer_precheck_blocks_limit_and_insufficient_balance() -> None:
    """任务 3：业务预检在权限之后、副作用之前，只判断不改账本。"""

    runtime, _, audit = build_runtime()
    context = base_context()
    before = balances()

    over_limit = invoke(
        runtime,
        ToolCall("call_limit", "transfer", transfer_args(amount=60_000.0)),
        context,
    )
    assert over_limit.ok is False
    assert over_limit.action is DecisionAction.DENY
    assert over_limit.code == "EXCEED_LIMIT"

    poor = invoke(
        runtime,
        ToolCall(
            "call_balance",
            "transfer",
            transfer_args(from_account=SMALL_ACCOUNT, amount=20_000.01),
        ),
        context,
    )
    assert poor.ok is False
    assert poor.code == "INSUFFICIENT_BALANCE"

    # 预检失败必须停在 decision 阶段，账本和执行次数都不能变化。
    assert balances() == before
    assert demo.SIDE_EFFECTS["transfer_executions"] == 0
    # 这一行是本用例的重点：审计里只有 decision 阶段的记录，没有 execution 阶段，
    # 证明预检失败时 handler 根本没被调用过，而不是"调用了但回滚了"。
    assert all(record.phase == "decision" for record in audit.records)


def test_transfer_requires_approval_then_executes_with_redaction() -> None:
    """任务 5 + 任务 6：高风险写操作先 CONFIRM，审批后执行，结果账号被脱敏。"""

    runtime, approvals, audit = build_runtime()
    context = base_context()
    arguments = transfer_args(amount=1_200.0)
    from_before = ACCOUNTS[(context.tenant_id, FROM_ACCOUNT)]
    to_before = ACCOUNTS[(context.tenant_id, TO_ACCOUNT)]

    pending = invoke(runtime, ToolCall("call_pending", "transfer", arguments), context)
    assert pending.action is DecisionAction.CONFIRM
    assert pending.code == "APPROVAL_REQUIRED"
    assert demo.SIDE_EFFECTS["transfer_executions"] == 0
    assert ACCOUNTS[(context.tenant_id, FROM_ACCOUNT)] == from_before

    approvals.approve("approval_transfer", context, "transfer", arguments)
    approved = invoke(
        runtime,
        ToolCall("call_done", "transfer", arguments),
        replace(context, approval_id="approval_transfer"),
    )

    assert approved.ok is True
    assert approved.action is DecisionAction.ALLOW
    assert approved.code == "OK"
    assert approved.content["txn_id"] == f"txn_{'call_done'[-6:]}"
    assert approved.content["amount"] == 1_200.0
    assert approved.content["status"] == "succeeded"

    # 结果脱敏：模型只能看到前缀加后四位。
    assert approved.content["from"] == "ACC-A-****3456"
    assert approved.content["to"] == "ACC-A-****4321"
    # 反向断言：明文账号不允许以任何形式出现在返回给模型的内容里。
    assert FROM_ACCOUNT not in str(approved.content)

    assert ACCOUNTS[(context.tenant_id, FROM_ACCOUNT)] == from_before - 1_200.0
    assert ACCOUNTS[(context.tenant_id, TO_ACCOUNT)] == to_before + 1_200.0
    assert demo.SIDE_EFFECTS["transfer_executions"] == 1

    codes = [(record.phase, record.code) for record in audit.records]
    assert ("decision", "APPROVAL_REQUIRED") in codes
    assert ("decision", "APPROVED") in codes
    assert ("execution", "OK") in codes
    # 审计只留参数名，不落明文参数值。
    # 审计只记参数名不记参数值，所以明文账号也不该出现在审计记录里。
    assert all("ACC-A-123456" not in str(record.argument_keys) for record in audit.records)


def test_transfer_approval_is_bound_to_canonical_arguments() -> None:
    """审批是一次性的，并且和工具、租户、用户、参数完全绑定。"""

    runtime, approvals, _ = build_runtime()
    context = base_context()
    approved_arguments = transfer_args(amount=1_000.0)
    approvals.approve("approval_bound", context, "transfer", approved_arguments)
    approved_context = replace(context, approval_id="approval_bound")
    before = balances()

    # 拿着为 1000 元签发的审批去执行 2000 元：摘要对不上，退回 confirm。
    # 这条断言是整个审批机制的核心——审批绑定的是具体参数，不是"这个工具"。
    tampered = invoke(
        runtime,
        ToolCall("call_tampered", "transfer", transfer_args(amount=2_000.0)),
        approved_context,
    )
    assert tampered.action is DecisionAction.CONFIRM
    assert tampered.code == "APPROVAL_REQUIRED"
    assert balances() == before

    # 同一份参数，换个人来用这张审批：同样退回 confirm。
    other_user = invoke(
        runtime,
        ToolCall("call_other_user", "transfer", approved_arguments),
        replace(approved_context, user_id="u_999"),
    )
    assert other_user.action is DecisionAction.CONFIRM
    assert balances() == before

    first = invoke(runtime, ToolCall("call_first", "transfer", approved_arguments), approved_context)
    assert first.ok is True

    # 重放刚刚用过的审批：一次性凭证已作废，仍然是 confirm。
    replayed = invoke(
        runtime, ToolCall("call_replay", "transfer", approved_arguments), approved_context
    )
    assert replayed.ok is False
    assert replayed.action is DecisionAction.CONFIRM
    assert demo.SIDE_EFFECTS["transfer_executions"] == 1


def test_transfer_timeout_reports_unknown_result(monkeypatch: pytest.MonkeyPatch) -> None:
    """任务 4：非幂等写操作超时后结果未知，且不能留下半成品副作用。"""

    # 单笔限额会先拦住 80000 以上的转账，这里临时放开限额才能走到 handler 的超时分支。
    # 为什么要改限额：预检的 50000 限额会先把 90000 的转账挡在 decide 第 5 步，
    # handler 里的超时分支（>80000）永远走不到。临时把限额放开，才能验证超时链路。
    # monkeypatch 改的是模块级变量，用例结束后 pytest 自动还原。
    monkeypatch.setattr(demo, "SINGLE_TRANSFER_LIMIT", 100_000.0)

    runtime, approvals, audit = build_runtime()
    context = base_context()
    arguments = transfer_args(amount=90_000.0)
    approvals.approve("approval_timeout", context, "transfer", arguments)
    before = balances()

    result = invoke(
        runtime,
        ToolCall("call_timeout", "transfer", arguments),
        replace(context, approval_id="approval_timeout"),
    )

    assert result.ok is False
    assert result.action is DecisionAction.DENY
    assert result.code == "TIMEOUT_UNKNOWN"
    # 超时后账本必须一分未动——因为 handler 里 sleep 排在改余额之前。
    assert balances() == before
    assert demo.SIDE_EFFECTS["transfer_executions"] == 0
    assert any(
        record.phase == "execution" and record.code == "TIMEOUT_UNKNOWN" for record in audit.records
    )
