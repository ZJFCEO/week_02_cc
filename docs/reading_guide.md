# 阅读指南：这套工具治理框架该怎么读

读这份代码的顺序不该是"从改动的地方开始"。整个转账工具只有四段业务代码，治理框架一行没动——所以先搞懂框架凭什么能让新工具只写这四段，再回头看那四段，才有收获。

全部行号对应 `tool_governance_demo.py` 与 `tests/test_tool_governance.py` 当前版本。

---

## 第 0 步：先跑起来，让输出当地图

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python pydantic pytest
.venv/bin/python tool_governance_demo.py
```

盯住输出里的 `call_07 / call_08 / call_09` 三行，转账的三种结局都在：

| 调用 | 结果 | 含义 |
|---|---|---|
| `call_07` | `confirm` `APPROVAL_REQUIRED` | 参数合法、权限齐全，但高风险写操作必须先问人 |
| `call_08` | `allow` `OK` | 拿着绑定参数的审批重放，执行成功，返回的账号已脱敏 |
| `call_09` | `deny` `EXCEED_LIMIT` | 同一份审批换了金额，先被业务预检挡住 |

后面每读一段代码，都回来和这三行对一次。

---

## 第 1 步：读类型，先别读逻辑（18–195 行）

这一段几乎没有业务逻辑，但它是全篇的设计交底。

| 读这里 | 带着这个问题 |
|---|---|
| `ExecutionContext` `:47` | 谁在调用？注意 `frozen=True`——handler 拿到它也改不了自己的权限 |
| `ToolPolicy` `:58` | 七个字段没有一个是业务参数，全是"这个工具有多危险" |
| `ToolDefinition.to_model_tool` `:110` | 模型只拿到 name / description / schema，`handler`、`policy`、`precheck` 一个都不给 |
| `DecisionAction` `:37` | 三态。`confirm` 不是失败，是"还没问人" |
| `PermissionDecision` `:131` | 每个拒绝都带 `code` 和 `source`，供上游程序判断，不是给人看的文案 |

一句话：治理信息是提前声明在类型里的，不是散落在 `if` 里的。

---

## 第 2 步：读主干，只看四条注释（455–523 行）

`ToolRuntime.invoke` 全长七十行，骨架就四步：

- `prepare-1` `:461` 把不可信字典变成业务对象
- `prepare-2` `:471` 执行期重新授权
- `execute` `:496` 此处之后才可能有副作用
- `finalize` `:507` 先脱敏，再写审计，最后才给模型

重点不是每步做了什么，是这个顺序不能交换：

- 先转类型再决策，决策函数就永远拿不到脏数据，不必再写一遍防御
- 决策不通过直接 `return`，handler 连被调用的机会都没有
- 脱敏排在生成 `ToolResult` 之前，模型没有任何机会看到明文

所有工具共享这一条主干，这就是"转账要不要审批"不需要我写的原因。

---

## 第 3 步：读 `decide` 的九步优先级（321–430 行）

这是整个框架的核心。九个编号注释按顺序读，每读一步问自己"能不能往上挪或往下挪"：

| 步骤 | 位置 | 为什么在这个位置 |
|---|---|---|
| 1 deny-first | `:327` | 排在最前，所以 `bypassPermissions` 也盖不掉硬拒绝 |
| 2 plan 模式 | `:334` | 只读契约写在执行层，而不是一句系统提示词 |
| 3 白名单重查 | `:343` | 发现期过滤过了，执行期再查一次——模型可能报一个没给它的工具名 |
| 4 RBAC | `:352` | 只相信认证层生成的 `ExecutionContext`，不信模型自称的身份 |
| 5 业务预检 | `:361` | 排在审批之前：余额不够就别去打扰人 |
| 6 审批 | `:369` | `requires_approval or risk is Risk.HIGH`，两个条件是或 |
| 7 bypass | `:391` | 排在审批之后：bypass 只能跳过普通确认，跳不过审批 |
| 8 allow 规则 | `:400` | allow 永远是最后生效的，不能反超前面任何一道 |
| 9 危险命令兜底 | `:407` | 正则只是教学兜底，生产要靠窄工具、AST 和沙箱 |

作业禁止改这个函数，不是怕改坏，是逼你去适配它——真实框架的优先级顺序就是这样定死的。

---

## 第 4 步：这时候才轮到转账的四段业务代码

| 位置 | 职责 | 关键细节 |
|---|---|---|
| `TransferArgs` `:88` | 第一道闸门 | 闸门放在类型层而不是函数开头；`extra="forbid"` 是防注入的最后屏障 |
| `transfer_precheck` `:675` | 只判断不改账本 | 两条规则都 `raise PolicyDenied`，被 `:361` 接住变成 business 拒绝 |
| `transfer_handler` `:687` | 全篇唯一允许有副作用的地方 | 转入账户检查排在扣款之前；超时的 `sleep` 排在改余额之前 |
| `build_tools` 注册 `:743` | 把工具接进框架 | 七个治理参数填完，审批、超时、脱敏、审计全部自动生效 |

回头数一下：这四段里没有一处 `if 需要审批`、没有一处调用脱敏、没有一处写审计。它们只回答"转账这件事的业务规则是什么"。

框架好不好，就看接入一个新工具时，你被迫写了多少与业务无关的代码。

---

## 第 5 步：把测试当链路切片读

五个测试就是链路上的五个观察点，每个只盯一环：

| 测试 | 位置 | 盯的是哪一环 |
|---|---|---|
| `test_transfer_rejects_invalid_arguments` | `:48` | 参数校验：格式、范围、额外字段 |
| `test_transfer_precheck_blocks_limit_and_insufficient_balance` | `:79` | 业务预检：限额与余额，且审计里只有 decision 阶段 |
| `test_transfer_requires_approval_then_executes_with_redaction` | `:113` | 审批放行 + 结果脱敏 + 审计三条记录 |
| `test_transfer_approval_is_bound_to_canonical_arguments` | `:159` | 审批绑定参数、绑定用户、一次性 |
| `test_transfer_timeout_reports_unknown_result` | `:197` | 非幂等写操作超时的语义 |

---

## 想学进去，就拆一层看一层

读懂治理框架最快的方式，是把某一层删掉，看哪个测试变红。下面每一行都实测过：

```bash
.venv/bin/python -m pytest tests/test_tool_governance.py -v -k transfer
```

### 实验 1：把审批摘掉

先只把 `:746` 的 `requires_approval` 从 `True` 改成 `False`——**五个测试依然全绿**。因为 `:369` 的条件是 `requires_approval or risk is Risk.HIGH`，`Risk.HIGH` 自己就足以触发审批。

把 `Risk.HIGH` 一起降成 `Risk.MEDIUM`，审批和审批绑定两个测试才会红。

> 结论：`requires_approval` 是给"低风险但仍需确认"的工具留的开关；高风险工具的审批是强制的，关不掉。

### 实验 2：删掉 `precheck=transfer_precheck`（`:748`）

预检测试变红，但钱没有立刻丢——六万的超限转账退化成了 `confirm APPROVAL_REQUIRED`，被审批层兜住了。真正的问题在下一步：人一旦点了确认，`allow OK`，余额从 100000 直接扣到 40000。

> 结论：少一层不一定马上漏钱，但语义会退化成"问人"，等于把本该由程序判断的限额推给了人。

### 实验 3：删掉账号脱敏（`:303` 改成 `return masked`）

审批+脱敏测试变红，明文账号直接进了模型上下文。

> 结论：脱敏是横切关注点，必须留在 runtime。写在 handler 里，下一个工具的作者一定会忘。

### 实验 4：把 `idempotent` 从 `False` 改成 `True`（`:746` 最后一个参数）

超时测试变红，错误码从 `TIMEOUT_UNKNOWN` 变成 `TIMEOUT`。

> 结论：超时该怎么报，由幂等性决定，不由超时本身决定。

### 实验 5：把 `sleep` 挪到改余额之后

超时测试里"账本未变"的断言变红，余额真的被改了（`100000 → 10000`、`5000 → 95000`），而调用方只收到一句超时。

> 结论：副作用的先后顺序决定了超时的后果。`TIMEOUT_UNKNOWN` 这个"未知"不是谦虚，是真的未知。

---

## 三条能带走的设计思路

1. **把"能不能做"和"怎么做"彻底分开。** `decide` 只回答前者，handler 只回答后者，两者之间用 `PolicyDenied` 和三态决策通信。业务代码里一旦出现权限判断，这条边界就破了。

2. **横切关注点一律上提到 runtime。** 脱敏、审计、超时、重试、幂等，没有一样应该由工具作者重新实现一遍。判断标准很简单：这件事如果新工具的作者忘了做会怎样？会出事，就说明它不该放在工具里。

3. **拒绝必须带机器可读的 code。** `EXCEED_LIMIT` 和 `INSUFFICIENT_BALANCE` 是给上游程序看的：模型看到前者会改金额重试，看到后者会换账户。只给一句中文错误描述，模型只能瞎猜。

---

## 附：文件地图

| 区段 | 行号 | 内容 |
|---|---|---|
| 枚举与数据类 | 18–195 | 权限模式、三态决策、执行上下文、工具策略与定义 |
| 审批与审计 | 198–275 | `PolicyDenied`、参数摘要、`ApprovalStore`、`AuditSink` |
| 脱敏 | 283–305 | 危险命令正则、`_redact`、账号掩码 |
| 权限引擎 | 307–430 | 九步优先级状态机（作业禁止改动） |
| 执行入口 | 433–577 | `ToolRuntime.invoke`、超时重试、拒绝与失败的审计落库 |
| 模拟数据 | 591–616 | `ORDERS`、`ACCOUNTS`、限额常量、副作用计数 |
| 工具实现 | 619–713 | 三个原有工具 + 转账的预检与 handler |
| 组装与演示 | 715–868 | `build_tools`、默认规则、执行上下文、离线演示 |

时序图见 [transfer_sequence.md](transfer_sequence.md)。
