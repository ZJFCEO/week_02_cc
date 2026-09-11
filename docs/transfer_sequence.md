# 转账工具治理链路时序图

对应实现：`tool_governance_demo.py` 的 `transfer` 工具，测试见 `tests/test_tool_governance.py`。

```mermaid
sequenceDiagram
    participant C as 调用方
    participant R as ToolRuntime
    participant P as 权限引擎
    participant H as transfer_handler
    participant A as AuditSink

    C->>R: invoke(transfer, args)
    R->>R: TransferArgs 校验 (extra=forbid)
    R->>P: decide()
    P->>P: deny 规则 / plan / 白名单 / RBAC
    P->>P: transfer_precheck 限额 + 余额
    P-->>R: confirm APPROVAL_REQUIRED
    R->>A: decision 记录
    R-->>C: ok=false action=confirm

    Note over C,P: 人工审批 approve(approval_id)

    C->>R: invoke(transfer, 同一份 args) + approval_id
    R->>P: decide()
    P->>P: 比对审批摘要 (用户/租户/工具/参数)
    P-->>R: allow APPROVED
    alt amount <= 80000
        R->>H: 执行 (2s 超时保护)
        H->>H: 扣款并入账
        H-->>R: txn_id + 明文账号
        R->>R: _redact 脱敏
        R->>A: execution OK + latency
        R-->>C: ACC-A-****3456
    else amount > 80000
        R->>H: 执行 (sleep 3s)
        H--xR: 超时取消，账本未变
        R->>A: execution TIMEOUT_UNKNOWN
        R-->>C: ok=false TIMEOUT_UNKNOWN
    end
```

## 三个关键点

1. **审批绑定参数**：`_approval_digest(tool_name, arguments)` 把用户、租户、工具名和全部参数一起做哈希。改一分钱、换个用户都会退回 `confirm`，并且审批用过即作废。
2. **脱敏在 runtime，不在 handler**：`transfer_handler` 返回明文账号，`ToolRuntime` 在 finalize 阶段统一走 `_redact`。放在 runtime 这一层，以后新增的工具才不会漏掉脱敏。
3. **超时返回 `TIMEOUT_UNKNOWN` 而不是 `TIMEOUT`**：`transfer` 的 `idempotent=False`。sleep 排在改余额之前，这次确实没扣钱，但 runtime 无权做这个假设，对调用方只能承认"结果未知"。
