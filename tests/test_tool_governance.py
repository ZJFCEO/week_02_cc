from __future__ import annotations

from dataclasses import replace

import pytest

from tool_governance_demo import (
    ApprovalStore,
    AuditSink,
    DecisionAction,
    PermissionMode,
    ToolCall,
    base_context,
    build_runtime,
    reset_side_effects,
    SIDE_EFFECTS,
)


REFUND_ARGUMENTS = {
    "order_id": "ord_1001",
    "amount": 399.0,
    "reason": "商品存在质量问题",
}


@pytest.mark.asyncio
async def test_deny_first_beats_bypass_permissions() -> None:
    reset_side_effects()
    runtime, _, _ = build_runtime()
    result = await runtime.invoke(
        ToolCall("deny_01", "run_shell", {"command": "rm -rf /tmp/demo"}),
        base_context(mode=PermissionMode.BYPASS_PERMISSIONS),
    )
    assert result.code == "DENY_RULE"
    assert SIDE_EFFECTS["shell_executions"] == 0


@pytest.mark.asyncio
async def test_plan_mode_denies_write_before_approval() -> None:
    reset_side_effects()
    approvals = ApprovalStore()
    runtime, _, _ = build_runtime(approvals=approvals)
    context = base_context(mode=PermissionMode.PLAN)
    approvals.approve("approval_plan", context, "create_refund", REFUND_ARGUMENTS)
    result = await runtime.invoke(
        ToolCall("plan_01", "create_refund", REFUND_ARGUMENTS),
        replace(context, approval_id="approval_plan"),
    )
    assert result.code == "PLAN_MODE_DENIED"
    assert SIDE_EFFECTS["refund_executions"] == 0


@pytest.mark.asyncio
async def test_schema_rejects_forged_identity_and_approval() -> None:
    reset_side_effects()
    runtime, _, _ = build_runtime()
    result = await runtime.invoke(
        ToolCall(
            "schema_01",
            "create_refund",
            {**REFUND_ARGUMENTS, "user_id": "admin", "approved": True},
        ),
        base_context(),
    )
    assert result.code == "INVALID_ARGUMENT"
    assert SIDE_EFFECTS["refund_executions"] == 0


@pytest.mark.asyncio
async def test_rbac_denial_keeps_handler_at_zero_calls() -> None:
    reset_side_effects()
    runtime, _, _ = build_runtime()
    result = await runtime.invoke(
        ToolCall("rbac_01", "create_refund", REFUND_ARGUMENTS),
        base_context(permissions=frozenset({"order:read"})),
    )
    assert result.code == "PERMISSION_DENIED"
    assert SIDE_EFFECTS["refund_executions"] == 0


@pytest.mark.asyncio
async def test_approval_is_bound_to_canonical_arguments() -> None:
    reset_side_effects()
    approvals = ApprovalStore()
    runtime, _, _ = build_runtime(approvals=approvals)
    context = base_context()
    approvals.approve(
        "approval_changed",
        context,
        "create_refund",
        {"order_id": "ord_1001", "amount": 100.0, "reason": "部分商品退款"},
    )
    result = await runtime.invoke(
        ToolCall("approval_01", "create_refund", REFUND_ARGUMENTS),
        replace(context, approval_id="approval_changed"),
    )
    assert result.action is DecisionAction.CONFIRM
    assert result.code == "APPROVAL_REQUIRED"
    assert SIDE_EFFECTS["refund_executions"] == 0


@pytest.mark.asyncio
async def test_result_is_redacted_but_audit_keeps_tool_call_id() -> None:
    reset_side_effects()
    audit = AuditSink()
    runtime, _, _ = build_runtime(audit=audit)
    result = await runtime.invoke(
        ToolCall("result_01", "get_order", {"order_id": "ord_1001"}),
        base_context(),
    )
    assert result.ok is True
    assert result.content == {
        "status": "paid",
        "refundable": 399.0,
        "customer_email": "***@***",
        "access_token": "***",
    }
    assert audit.records[-1].tool_call_id == "result_01"
    assert audit.records[-1].decision == "executed"


@pytest.mark.asyncio
async def test_discovery_and_execution_both_enforce_whitelist() -> None:
    reset_side_effects()
    runtime, _, _ = build_runtime()
    context = base_context(allowed_tools=frozenset({"get_order"}))
    model_names = {item["function"]["name"] for item in runtime.model_tools(context)}
    assert model_names == {"get_order"}

    result = await runtime.invoke(
        ToolCall("stale_01", "create_refund", REFUND_ARGUMENTS),
        context,
    )
    assert result.code == "TOOL_NOT_ALLOWED"
    assert SIDE_EFFECTS["refund_executions"] == 0


@pytest.mark.asyncio
async def test_one_time_approval_cannot_be_replayed() -> None:
    reset_side_effects()
    approvals = ApprovalStore()
    runtime, _, _ = build_runtime(approvals=approvals)
    context = base_context()
    approvals.approve("approval_once", context, "create_refund", REFUND_ARGUMENTS)
    approved_context = replace(context, approval_id="approval_once")

    first = await runtime.invoke(ToolCall("once_01", "create_refund", REFUND_ARGUMENTS), approved_context)
    second = await runtime.invoke(ToolCall("once_02", "create_refund", REFUND_ARGUMENTS), approved_context)

    assert first.ok is True
    assert second.action is DecisionAction.CONFIRM
    assert second.code == "APPROVAL_REQUIRED"
    assert SIDE_EFFECTS["refund_executions"] == 1

# ============================================================================
# 第二章作业：转账工具测试
#
# 以上 8 个用例为训练营原版，逐字未改。以下 5 个为作业新增，
# 写法沿用原版：async 用例 + 开头 reset_side_effects() + 全部走 runtime.invoke。
#
# 转账测试额外用到账本 ACCOUNTS 和可临时放开的限额常量，
# 为了不改动原版的 import 块，导入放在这里。
# ============================================================================

import tool_governance_demo as demo  # noqa: E402
from tool_governance_demo import ACCOUNTS  # noqa: E402

TRANSFER_ARGUMENTS = {
    "from_account": "ACC-A-123456",
    "to_account": "ACC-A-654321",
    "amount": 1_000.0,
}


@pytest.mark.asyncio
async def test_transfer_schema_rejects_invalid_arguments() -> None:
    # 任务 2：格式、范围、额外字段都挡在参数校验这一层，handler 碰都碰不到。
    reset_side_effects()
    runtime, _, _ = build_runtime()
    before = dict(ACCOUNTS)

    invalid_arguments = [
        {**TRANSFER_ARGUMENTS, "from_account": "ACC-a-123456"},  # 小写字母，正则不匹配
        {**TRANSFER_ARGUMENTS, "to_account": "ACC-A-12345"},  # 只有 5 位数字
        {**TRANSFER_ARGUMENTS, "amount": 0.0},  # 金额必须大于 0
        {**TRANSFER_ARGUMENTS, "amount": 100_000.01},  # 超过 Schema 上限
        {**TRANSFER_ARGUMENTS, "approved": True},  # 模型注入越权字段，extra="forbid" 拦截
    ]
    for index, arguments in enumerate(invalid_arguments):
        result = await runtime.invoke(
            ToolCall(f"transfer_schema_{index}", "transfer", arguments),
            base_context(),
        )
        assert result.action is DecisionAction.DENY
        assert result.code == "INVALID_ARGUMENT"

    assert dict(ACCOUNTS) == before
    assert SIDE_EFFECTS["transfer_executions"] == 0


@pytest.mark.asyncio
async def test_transfer_precheck_denies_over_limit_and_insufficient_balance() -> None:
    # 任务 3：业务预检只判断不改账本，拒绝必须停在 decision 阶段。
    reset_side_effects()
    audit = AuditSink()
    runtime, _, _ = build_runtime(audit=audit)
    before = dict(ACCOUNTS)

    over_limit = await runtime.invoke(
        ToolCall("transfer_limit_01", "transfer", {**TRANSFER_ARGUMENTS, "amount": 60_000.0}),
        base_context(),
    )
    assert over_limit.action is DecisionAction.DENY
    assert over_limit.code == "EXCEED_LIMIT"

    # ACC-A-888888 余额 20000，转 20000.01 差一分钱
    insufficient = await runtime.invoke(
        ToolCall(
            "transfer_balance_01",
            "transfer",
            {**TRANSFER_ARGUMENTS, "from_account": "ACC-A-888888", "amount": 20_000.01},
        ),
        base_context(),
    )
    assert insufficient.action is DecisionAction.DENY
    assert insufficient.code == "INSUFFICIENT_BALANCE"

    assert dict(ACCOUNTS) == before
    assert SIDE_EFFECTS["transfer_executions"] == 0
    # 审计里只有 decision 阶段：证明 handler 根本没被调用，而不是调用后回滚
    assert all(record.phase == "decision" for record in audit.records)


@pytest.mark.asyncio
async def test_transfer_requires_approval_then_executes_with_redacted_accounts() -> None:
    # 任务 5 + 任务 6：高风险写操作先 CONFIRM，审批后执行，返回给模型的账号已脱敏。
    reset_side_effects()
    approvals = ApprovalStore()
    audit = AuditSink()
    runtime, _, _ = build_runtime(approvals=approvals, audit=audit)
    context = base_context()
    arguments = {**TRANSFER_ARGUMENTS, "amount": 1_200.0}
    from_before = ACCOUNTS[("tenant_a", "ACC-A-123456")]
    to_before = ACCOUNTS[("tenant_a", "ACC-A-654321")]

    pending = await runtime.invoke(ToolCall("transfer_pending", "transfer", arguments), context)
    assert pending.action is DecisionAction.CONFIRM
    assert pending.code == "APPROVAL_REQUIRED"
    assert SIDE_EFFECTS["transfer_executions"] == 0

    approvals.approve("approval_transfer", context, "transfer", arguments)
    approved = await runtime.invoke(
        ToolCall("transfer_approved", "transfer", arguments),
        replace(context, approval_id="approval_transfer"),
    )

    assert approved.ok is True
    assert approved.content == {
        "txn_id": "txn_proved",  # tool_call_id 后 6 位
        "from": "ACC-A-****3456",
        "to": "ACC-A-****4321",
        "amount": 1_200.0,
        "status": "succeeded",
    }
    assert ACCOUNTS[("tenant_a", "ACC-A-123456")] == from_before - 1_200.0
    assert ACCOUNTS[("tenant_a", "ACC-A-654321")] == to_before + 1_200.0
    assert SIDE_EFFECTS["transfer_executions"] == 1

    # 审计还原出"挂起 -> 审批放行 -> 执行成功"这条时间线
    assert [(record.phase, record.code) for record in audit.records] == [
        ("decision", "APPROVAL_REQUIRED"),
        ("decision", "APPROVED"),
        ("execution", "OK"),
    ]
    assert audit.records[-1].tool_call_id == "transfer_approved"


@pytest.mark.asyncio
async def test_transfer_approval_is_bound_to_canonical_arguments() -> None:
    # 审批绑定具体参数和具体的人：改金额、换用户都作废，并且只能用一次。
    reset_side_effects()
    approvals = ApprovalStore()
    runtime, _, _ = build_runtime(approvals=approvals)
    context = base_context()
    approvals.approve("approval_bound", context, "transfer", TRANSFER_ARGUMENTS)
    approved_context = replace(context, approval_id="approval_bound")

    # 拿着为 1000 元签发的审批去转 2000 元
    tampered = await runtime.invoke(
        ToolCall("transfer_tampered", "transfer", {**TRANSFER_ARGUMENTS, "amount": 2_000.0}),
        approved_context,
    )
    assert tampered.action is DecisionAction.CONFIRM
    assert tampered.code == "APPROVAL_REQUIRED"

    # 同一份参数，换个人来用这张审批
    other_user = await runtime.invoke(
        ToolCall("transfer_other_user", "transfer", TRANSFER_ARGUMENTS),
        replace(approved_context, user_id="u_999"),
    )
    assert other_user.action is DecisionAction.CONFIRM
    assert SIDE_EFFECTS["transfer_executions"] == 0

    first = await runtime.invoke(ToolCall("transfer_first", "transfer", TRANSFER_ARGUMENTS), approved_context)
    second = await runtime.invoke(ToolCall("transfer_replay", "transfer", TRANSFER_ARGUMENTS), approved_context)

    assert first.ok is True
    assert second.action is DecisionAction.CONFIRM  # 一次性凭证已核销
    assert SIDE_EFFECTS["transfer_executions"] == 1


@pytest.mark.asyncio
async def test_transfer_timeout_reports_unknown_without_side_effects(monkeypatch: pytest.MonkeyPatch) -> None:
    # 任务 4：非幂等写操作超时返回 TIMEOUT_UNKNOWN，且不能留下半成品副作用。
    reset_side_effects()
    # 限额 50000 会在预检阶段先挡住 80000 以上的转账，handler 的超时分支永远走不到；
    # 临时放开限额才能验证超时链路，用例结束后 monkeypatch 自动还原。
    monkeypatch.setattr(demo, "SINGLE_TRANSFER_LIMIT", 100_000.0)

    approvals = ApprovalStore()
    audit = AuditSink()
    runtime, _, _ = build_runtime(approvals=approvals, audit=audit)
    context = base_context()
    arguments = {**TRANSFER_ARGUMENTS, "amount": 90_000.0}
    approvals.approve("approval_timeout", context, "transfer", arguments)
    before = dict(ACCOUNTS)

    result = await runtime.invoke(
        ToolCall("transfer_timeout", "transfer", arguments),
        replace(context, approval_id="approval_timeout"),
    )

    assert result.ok is False
    assert result.code == "TIMEOUT_UNKNOWN"
    # handler 里 sleep 排在改余额之前，超时取消后账本一分未动
    assert dict(ACCOUNTS) == before
    assert SIDE_EFFECTS["transfer_executions"] == 0
    assert audit.records[-1].phase == "execution"
    assert audit.records[-1].code == "TIMEOUT_UNKNOWN"
