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