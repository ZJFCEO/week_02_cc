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


@pytest.fixture(autouse=True)
def _clean_state() -> None:
    reset_side_effects()
    yield
    reset_side_effects()


def invoke(runtime, call: ToolCall, context) -> Any:
    return asyncio.run(runtime.invoke(call, context))


def transfer_args(**overrides: Any) -> dict[str, Any]:
    return {"from_account": FROM_ACCOUNT, "to_account": TO_ACCOUNT, "amount": 1_000.0, **overrides}


def balances() -> dict[tuple[str, str], float]:
    return dict(ACCOUNTS)


def test_transfer_rejects_invalid_arguments() -> None:
    """任务 2：Schema 是第一道闸门，格式、范围与额外字段都进不了 handler。"""

    runtime, _, _ = build_runtime()
    context = base_context()
    before = balances()

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
    assert FROM_ACCOUNT not in str(approved.content)

    assert ACCOUNTS[(context.tenant_id, FROM_ACCOUNT)] == from_before - 1_200.0
    assert ACCOUNTS[(context.tenant_id, TO_ACCOUNT)] == to_before + 1_200.0
    assert demo.SIDE_EFFECTS["transfer_executions"] == 1

    codes = [(record.phase, record.code) for record in audit.records]
    assert ("decision", "APPROVAL_REQUIRED") in codes
    assert ("decision", "APPROVED") in codes
    assert ("execution", "OK") in codes
    # 审计只留参数名，不落明文参数值。
    assert all("ACC-A-123456" not in str(record.argument_keys) for record in audit.records)


def test_transfer_approval_is_bound_to_canonical_arguments() -> None:
    """审批是一次性的，并且和工具、租户、用户、参数完全绑定。"""

    runtime, approvals, _ = build_runtime()
    context = base_context()
    approved_arguments = transfer_args(amount=1_000.0)
    approvals.approve("approval_bound", context, "transfer", approved_arguments)
    approved_context = replace(context, approval_id="approval_bound")
    before = balances()

    tampered = invoke(
        runtime,
        ToolCall("call_tampered", "transfer", transfer_args(amount=2_000.0)),
        approved_context,
    )
    assert tampered.action is DecisionAction.CONFIRM
    assert tampered.code == "APPROVAL_REQUIRED"
    assert balances() == before

    other_user = invoke(
        runtime,
        ToolCall("call_other_user", "transfer", approved_arguments),
        replace(approved_context, user_id="u_999"),
    )
    assert other_user.action is DecisionAction.CONFIRM
    assert balances() == before

    first = invoke(runtime, ToolCall("call_first", "transfer", approved_arguments), approved_context)
    assert first.ok is True

    replayed = invoke(
        runtime, ToolCall("call_replay", "transfer", approved_arguments), approved_context
    )
    assert replayed.ok is False
    assert replayed.action is DecisionAction.CONFIRM
    assert demo.SIDE_EFFECTS["transfer_executions"] == 1


def test_transfer_timeout_reports_unknown_result(monkeypatch: pytest.MonkeyPatch) -> None:
    """任务 4：非幂等写操作超时后结果未知，且不能留下半成品副作用。"""

    # 单笔限额会先拦住 80000 以上的转账，这里临时放开限额才能走到 handler 的超时分支。
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
    assert balances() == before
    assert demo.SIDE_EFFECTS["transfer_executions"] == 0
    assert any(
        record.phase == "execution" and record.code == "TIMEOUT_UNKNOWN" for record in audit.records
    )
