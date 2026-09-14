# 阅读指南：这套工具治理框架该怎么读

读这份代码的顺序不该是"从改动的地方开始"。整个转账工具只有四段业务代码，治理框架一行没动——
所以先搞懂框架凭什么能让新工具只写这四段，再回头看那四段，才有收获。

## 先说怎么定位

本文一律用**函数名和章节标题**指路，不用行号——行号会随着每次改动漂掉，靠不住。

文件里留了十个章节分隔符，这条命令随时打印出当前的目录和行号：

```bash
grep -nE "^# ===|^(class|def|async def) |^    (async def|def) .*\(" tool_governance_demo.py
```

编辑器里直接搜章节标题（比如 `六、权限状态机`）或函数名（比如 `async def decide`）也一样。

---

## 第 0 步：先跑起来，让输出当地图

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python pydantic pytest pytest-asyncio
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

## 第 1 步：读类型，先别读逻辑

> 位置：章节 **一、治理用到的枚举与常量** 到 **四、工具定义与框架内部结构**

这一段几乎没有业务逻辑，但它是全篇的设计交底。

| 读这个 | 带着这个问题 |
|---|---|
| `class ExecutionContext` | 谁在调用？注意 `frozen=True`——handler 拿到它也改不了自己的权限 |
| `class ToolPolicy` | 七个字段没有一个是业务参数，全是"这个工具有多危险" |
| `def to_model_tool` | 模型只拿到 name / description / schema，`handler`、`policy`、`precheck` 一个都不给 |
| `class DecisionAction` | 三态。`confirm` 不是失败，是"还没问人" |
| `class PermissionDecision` | 每个拒绝都带 `code` 和 `source`，供上游程序判断，不是给人看的文案 |

一句话：治理信息是提前声明在类型里的，不是散落在 `if` 里的。

---

## 第 2 步：读主干，只看四条注释

> 位置：章节 **七、执行入口**，`async def invoke`

`ToolRuntime.invoke` 全长七十行，骨架就四步，在代码里搜这四个关键词即可：

| 搜 | 做什么 |
|---|---|
| `prepare-1` | 把不可信字典变成业务对象 |
| `prepare-2` | 执行期重新授权 |
| `execute` | 此处之后才可能有副作用 |
| `finalize` | 先脱敏，再写审计，最后才给模型 |

重点不是每步做了什么，是这个顺序不能交换：

- 先转类型再决策，决策函数就永远拿不到脏数据，不必再写一遍防御
- 决策不通过直接 `return`，handler 连被调用的机会都没有
- 脱敏排在生成 `ToolResult` 之前，模型没有任何机会看到明文

所有工具共享这一条主干，这就是"转账要不要审批"不需要我写的原因。

---

## 第 3 步：读 `decide` 的九步优先级

> 位置：章节 **六、权限状态机**，`async def decide`

这是整个框架的核心。九个步骤的注释都以 `# 1.` `# 2.` 这样编号，按顺序读一遍，
每读一步问自己"能不能往上挪或往下挪"：

| 步骤 | 为什么在这个位置 |
|---|---|
| 1 deny 规则 | 排在最前，所以 `bypassPermissions` 也盖不掉硬拒绝 |
| 2 plan 模式 | 只读契约写在执行层，而不是一句系统提示词 |
| 3 白名单重查 | 发现期过滤过了，执行期再查一次——模型可能报一个没给它的工具名 |
| 4 RBAC | 只相信认证层生成的 `ExecutionContext`，不信模型自称的身份 |
| 5 业务预检 | 排在审批之前：余额不够就别去打扰人 |
| 6 审批 | 条件是 `requires_approval or risk is Risk.HIGH`，两者是或 |
| 7 bypass | 排在审批之后：只能跳过普通确认，跳不过审批 |
| 8 allow 规则 | 永远最后生效，不能反超前面任何一道 |
| 9 危险命令兜底 | 正则只是教学兜底，生产要靠窄工具、AST 和沙箱 |

作业禁止改这个函数，不是怕改坏，是逼你去适配它——真实框架的优先级顺序就是这样定死的。

---

## 第 4 步：这时候才轮到转账的四段业务代码

> 位置：章节 **八、模拟数据与工具实现** 与 **九、工具注册与运行时组装**

| 读这个 | 职责 | 关键细节 |
|---|---|---|
| `class TransferArgs` | 第一道闸门 | 闸门放在类型层而不是函数开头；`extra="forbid"` 是防注入的最后屏障 |
| `async def transfer_precheck` | 只判断不改账本 | 两条规则都 `raise PolicyDenied`，被 `decide` 第 5 步接住变成 business 拒绝 |
| `async def transfer_handler` | 全篇唯一允许有副作用的地方 | 转入账户检查排在扣款之前；超时的 `sleep` 排在改余额之前 |
| `build_tools()` 里 `name="transfer"` | 把工具接进框架 | 七个治理参数填完，审批、超时、脱敏、审计全部自动生效 |

回头数一下：这四段里没有一处 `if 需要审批`、没有一处调用脱敏、没有一处写审计。
它们只回答"转账这件事的业务规则是什么"。

框架好不好，就看接入一个新工具时，你被迫写了多少与业务无关的代码。

---

## 第 5 步：把测试当链路切片读

五个测试就是链路上的五个观察点，每个只盯一环：

| 测试函数 | 盯的是哪一环 |
|---|---|
| `test_transfer_rejects_invalid_arguments` | 参数校验：格式、范围、额外字段 |
| `test_transfer_precheck_blocks_limit_and_insufficient_balance` | 业务预检：限额与余额，且审计里只有 decision 阶段 |
| `test_transfer_requires_approval_then_executes_with_redaction` | 审批放行 + 结果脱敏 + 审计三条记录 |
| `test_transfer_approval_is_bound_to_canonical_arguments` | 审批绑定参数、绑定用户、一次性 |
| `test_transfer_timeout_reports_unknown_result` | 非幂等写操作超时的语义 |

---

## 想学进去，就拆一层看一层

读懂治理框架最快的方式，是把某一层删掉，看哪个测试变红。下面每一条都实测过：

```bash
.venv/bin/python -m pytest tests/test_tool_governance.py -v -k transfer
```

下面提到的"转账那条注册"，都指 `build_tools()` 里 `name="transfer"` 的那个 `ToolDefinition`。

### 实验 1：把审批摘掉

先只把转账那条注册里 `policy=ToolPolicy(...)` 的 `requires_approval` 从 `True` 改成 `False`
（第四个位置参数）——**五个测试依然全绿**。
因为 `decide` 第 6 步的条件是 `requires_approval or risk is Risk.HIGH`，`Risk.HIGH` 自己就足以触发审批。

把 `Risk.HIGH` 一起降成 `Risk.MEDIUM`，审批和审批绑定两个测试才会红。

> 结论：`requires_approval` 是给"低风险但仍需确认"的工具留的开关；高风险工具的审批是强制的，关不掉。

### 实验 2：删掉 `precheck=transfer_precheck`

预检测试变红，但钱没有立刻丢——六万的超限转账退化成了 `confirm APPROVAL_REQUIRED`，
被审批层兜住了。真正的问题在下一步：人一旦点了确认，`allow OK`，余额从 100000 直接扣到 40000。

> 结论：少一层不一定马上漏钱，但语义会退化成"问人"，等于把本该由程序判断的限额推给了人。

### 实验 3：删掉账号脱敏

在 `_redact` 里把 `ACCOUNT_PATTERN.sub(...)` 那一行改成 `return masked`。
审批+脱敏测试变红，明文账号直接进了模型上下文。

> 结论：脱敏是横切关注点，必须留在 runtime。写在 handler 里，下一个工具的作者一定会忘。

### 实验 4：把 `idempotent` 从 `False` 改成 `True`

转账那条注册里 `ToolPolicy(...)` 的最后一个位置参数。
超时测试变红，错误码从 `TIMEOUT_UNKNOWN` 变成 `TIMEOUT`。

> 结论：超时该怎么报，由幂等性决定，不由超时本身决定。

### 实验 5：把 `sleep` 挪到改余额之后

在 `transfer_handler` 里，把 `await asyncio.sleep(3.0)` 那两行移到 `ACCOUNTS[to_key] += ...` 之后。
超时测试里"账本未变"的断言变红，余额真的被改了（`100000 → 10000`、`5000 → 95000`），
而调用方只收到一句超时。

> 结论：副作用的先后顺序决定了超时的后果。`TIMEOUT_UNKNOWN` 这个"未知"不是谦虚，是真的未知。

---

## 三条能带走的设计思路

1. **把"能不能做"和"怎么做"彻底分开。** `decide` 只回答前者，handler 只回答后者，
   两者之间用 `PolicyDenied` 和三态决策通信。业务代码里一旦出现权限判断，这条边界就破了。

2. **横切关注点一律上提到 runtime。** 脱敏、审计、超时、重试、幂等，没有一样应该由工具作者重新实现一遍。
   判断标准很简单：这件事如果新工具的作者忘了做会怎样？会出事，就说明它不该放在工具里。

3. **拒绝必须带机器可读的 code。** `EXCEED_LIMIT` 和 `INSUFFICIENT_BALANCE` 是给上游程序看的：
   模型看到前者会改金额重试，看到后者会换账户。只给一句中文错误描述，模型只能瞎猜。

---

## 附：文件分区

`tool_governance_demo.py` 里用分隔注释划了十个区，搜章节标题即可跳转：

| 章节 | 内容 |
|---|---|
| 一、治理用到的枚举与常量 | 权限模式、副作用类型、风险等级、三态决策 |
| 二、一次调用的上下文与策略 | `ExecutionContext`、`ToolPolicy` |
| 三、工具参数模型 | `StrictArgs` 与四个工具的参数模型 |
| 四、工具定义与框架内部结构 | `ToolDefinition`、规则、决策、调用、结果、审计记录、异常 |
| 五、审批、审计与脱敏 | 参数摘要、`ApprovalStore`、`AuditSink`、`_redact` |
| 六、权限状态机 | 九步固定优先级（作业禁止改动） |
| 七、执行入口 | `ToolRuntime.invoke` 的四个阶段、超时重试、失败出口 |
| 八、模拟数据与工具实现 | `ORDERS`、`ACCOUNTS`、四个工具的 precheck 与 handler |
| 九、工具注册与运行时组装 | `build_tools`、默认规则、执行上下文、运行时装配 |
| 十、离线演示与真实 Agent Loop | `run_offline_demo`、可选的 DeepSeek 闭环 |

时序图见 [transfer_sequence.md](transfer_sequence.md)。
