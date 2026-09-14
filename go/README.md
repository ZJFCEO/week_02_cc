# Go 版：工具治理与权限状态机

`tool_governance_demo.py` 的 Go 复刻，包含第二章作业的转账工具，以及与 Python 版一一对应的测试。

行为与 Python 版对齐：同样的九次演示调用、同样的错误码、同样的脱敏结果、同样的审计条数。
但有三处语言层面的真实差异，注释里都标了 `【Go 差异】`，那才是这份复刻值得读的地方。

## 快速开始

离线演示，不需要 API Key：

```bash
go run ./cmd/demo
```

接 DeepSeek 跑真实 Agent Loop，对应 Python 版的 `--agent`，环境变量规则也一致
（`DEEPSEEK_API_KEY` 必填，`DEEPSEEK_BASE_URL`、`DEEPSEEK_MODEL` 可选）：

```bash
go run ./cmd/demo --agent --input "请查询订单 ord_1001 的状态和可退金额"
```

同样的 Agent Loop，改用 CloudWeGo Eino 框架实现（见下文「Eino 版」）：

```bash
go run ./cmd/eino-agent --input "请查询订单 ord_1001 的状态和可退金额"
```

对应作业验收命令，只跑 5 个转账测试：

```bash
go test ./governance/ -v -run Transfer
```

跑全部 25 个（`governance` 19 个 + `einoagent` 6 个）：

```bash
go test ./... -race -v
```

## 测试

Go 的测试文件按惯例和源码放在同一个目录（`governance/`），文件名以 `_test.go` 结尾，
不单独建 `tests/` 目录——这样测试能直接访问包内未导出的函数和变量。

| 文件 | 用例数 | 对应 Python 版 |
|---|---|---|
| `governance_test.go` | 8 | `tests/test_tool_governance.py` 前 8 个训练营原版用例，逐个移植 |
| `transfer_test.go` | 5 | 同一文件末尾的 5 个转账测试 |
| `timeout_semantics_test.go` | 1 | Go 独有，证明超时不等于取消 |
| `agent_test.go` | 5 | Go 独有，用本地假服务器模拟流式响应，测手写版 Agent Loop |
| `einoagent/tools_test.go` | 3 | Eino 版：用 Eino 真实的 ToolsNode 验证治理层仍是唯一执行入口 |
| `einoagent/agent_test.go` | 3 | Eino 版：假服务器驱动 ReAct Agent，含默认检查函数漏调工具的对照实验 |

函数名与 Python 一一对应，只是 snake_case 换成 CamelCase，
例如 `test_plan_mode_denies_write_before_approval` → `TestPlanModeDeniesWriteBeforeApproval`。
标了 `【Go 差异】` 的断言是 Go 版额外加的：转账参数多测一种"金额写成字符串"，超时测试多断言一次等待时长。

这套测试做过反向验证：把 deny 规则、plan 检查、`DisallowUnknownFields`、RBAC、审批摘要、执行期白名单、
审批核销、token 脱敏、账号脱敏、转账幂等性这 10 处逐一改坏，每一处都有对应的用例变红。

## 包结构

```
go/
├── cmd/
│   ├── demo/main.go                  离线演示 + --agent 手写版 Agent Loop
│   └── eino-agent/main.go            Eino 版 Agent Loop
├── einoagent/                        Eino 版（依赖 eino / eino-ext）
│   ├── tools.go                      适配器：Eino 工具 -> runtime.Invoke
│   ├── agent.go                      DeepSeek ChatModel + ReAct Agent
│   ├── tools_test.go
│   └── agent_test.go
└── governance/                       治理框架（只依赖标准库）
    ├── types.go                      枚举、执行上下文、工具策略、工具定义、结果与审计记录
    ├── errors.go                     PolicyError / TransientError / ValidationError
    ├── args.go                       Args 接口、四个参数结构体、DecodeArgs（extra=forbid 的等价物）
    ├── approval.go                   参数摘要、ApprovalStore（一次性 + 参数绑定）
    ├── audit.go                      AuditSink、危险命令黑名单、Redact 脱敏
    ├── engine.go                     PermissionEngine.Decide —— 九步固定优先级
    ├── runtime.go                    ToolRuntime.Invoke —— 一次调用的四个阶段
    ├── tools.go                      模拟数据、四个工具实现、注册与运行时组装
    ├── demo.go                       九次演示调用
    ├── agent.go                      手写版 Agent Loop（标准库手写流式解析）
    ├── helpers_test.go               测试公共工具 + go test 与 pytest 对照
    ├── governance_test.go            训练营原版 8 个基线测试（从 Python 移植）
    ├── transfer_test.go              作业新增的 5 个转账测试
    ├── timeout_semantics_test.go     Go 独有：证明超时不等于取消
    └── agent_test.go                 Go 独有：假服务器驱动的 Agent Loop 测试
```

## 作业六个任务的实现位置

| 任务 | 位置 | 说明 |
|---|---|---|
| 1 模拟账户数据 | `tools.go` 的 `accounts` | key 是 `AccountKey{Tenant, Account}` 结构体，对应 Python 的元组键 |
| 2 转账参数模型 | `args.go` 的 `TransferArgs` | 结构体 + `Validate()`，`DecodeArgs` 负责拦未知字段 |
| 3 业务预检 | `tools.go` 的 `transferPrecheck` | 先限额后余额，返回 `*PolicyError` 带错误码 |
| 4 转账处理 | `tools.go` 的 `transferHandler` | 超时模拟、转入账户校验、锁内一扣一加 |
| 5 注册工具 | `tools.go` 的 `BuildTools` | `ToolPolicy` 用具名字段，比位置参数不易出错 |
| 6 结果脱敏 | `audit.go` 的 `accountMaskPattern` | `ACC-A-123456 → ACC-A-****3456` |

## 三处和 Python 版不一样的地方

### 1. 参数校验：没有 pydantic，拆成两步

Python 一个 `Field(pattern=...)` 就把正则、范围、未知字段全办了。Go 这边：

| Python | Go |
|---|---|
| `extra="forbid"` | `json.Decoder.DisallowUnknownFields()` |
| `strict=True` | `encoding/json` 本来就不做隐式类型转换 |
| `Field(pattern=..., gt=..., le=...)` | 手写 `Validate() error` |
| 自动生成 JSON Schema | 手写 `Parameters map[string]any` |

一个细节差异：pydantic 会把所有字段的问题一次报全，`encoding/json` 遇到第一个未知字段就停。
所以 Go 版里结构性错误最多报一条，业务校验才是一次报全的。

### 2. 超时：Go 的超时是"放弃等待"，不是"取消执行"

这是最重要的一条。Python 的 `asyncio.timeout` 能把协程真正取消在它当时停住的那个 `await` 上，
后面的代码一行都不会执行。Go 不能从外部中断 goroutine，`context` 只是一个"请你自己停下来"的信号。

后果是：**handler 如果不看 `ctx.Done()`，运行时的超时拦不住它的副作用。**

`timeout_semantics_test.go` 用一个故意不看 ctx 的 handler 把这件事跑了出来：
`Invoke` 在 50 毫秒超时后就返回了 `TIMEOUT_UNKNOWN`，而那个 goroutine 在 300 毫秒后照样把副作用做了出来。

所以这份实现两条腿走路：

- `transferHandler` 里的等待写成 `select { case <-time.After(3s): case <-ctx.Done(): return nil, ctx.Err() }`，尊重取消信号
- 非幂等写操作对外只承认 `TIMEOUT_UNKNOWN`，让调用方带幂等键去查证，而不是直接重试

换句话说，`TIMEOUT_UNKNOWN` 这个错误码在 Go 里比在 Python 里更名副其实。

### 3. 并发：Python 不用加锁，Go 必须加

Python 版的账本、审批库、审计口都没有锁，因为 asyncio 是单线程事件循环。
Go 的 handler 可能被多个 goroutine 并发调用，`go test` 也可能并行跑用例，所以：

- `ledgerMu` 保护账本和副作用计数器
- `ApprovalStore` 和 `AuditSink` 各自带 `sync.Mutex`
- `AuditSink.Records()` 返回拷贝而不是内部切片

`go test -race` 是干净的。真实系统里账本这层锁的位置应该是数据库事务。

### 4. Agent Loop：不用 SDK，手写流式解析

Python 版用 `openai` SDK，流式解析和 tool_calls 拼接都被 SDK 藏起来了。
Go 版只用标准库 `net/http`，把 SDK 底下的事摊开来写（见 `agent.go` 的 `streamChat`）：

- 流式响应是一行行 `data: {json}`，最后一行 `data: [DONE]`；冒号开头的行是 keep-alive 注释，要跳过
- 一次工具调用的 `id`、`name`、`arguments` 会拆成好几个 chunk 到达，要按 `index` **追加**拼接，而不是覆盖
- 拼好后按 `index` 排序再执行——Go 的 map 遍历顺序是随机的，不排序的话每次执行顺序都可能不同

`agent_test.go` 用 `httptest` 起一个本地假服务器，故意把参数切成两段、插入 keep-alive 行和没有 choices 的用量 chunk，
并让模型越权调用 `create_refund`。测试验证了四件事：拼接正确；越权调用被白名单拦下，handler 一次没执行；
回填给模型的查单结果里只有 `***@***`，明文邮箱从未进入模型上下文；第 8 轮之后强制停止。

两边都接真实 DeepSeek 跑过同一句输入，行为一致。唯一可见的差别是数字格式：
Python 的 `json.dumps` 把可退金额写成 `399.0`，Go 的 `encoding/json` 写成 `399`，所以模型复述时也跟着不同。

## Eino 版

`einoagent/` 用 [CloudWeGo Eino](https://github.com/cloudwego/eino)（`v0.9.19`）重写了同一个 Agent Loop，
模型用 `eino-ext` 的 DeepSeek 组件（`v0.1.7`），编排用 ReAct Agent。

**分工很明确：Eino 管"跟模型打交道"，治理框架管"工具能不能执行"。** 两层是上下关系，不是二选一。
依赖也只进了 `einoagent` 包，`governance` 包仍然只用标准库。

| 手写版 `governance/agent.go` | Eino 版 `einoagent/` |
|---|---|
| `net/http` + 手写 SSE 解析 | `deepseek.NewChatModel` |
| 按 index 拼接 tool_calls 分片 | 框架内部完成 |
| 8 轮 for 循环 | `react.NewAgent`，`MaxStep: 16` |
| `ModelTools()` 输出 JSON Schema | `GovernedTool.Info()` 返回 `schema.ToolInfo` |
| 直接调 `runtime.Invoke` | `GovernedTool.InvokableRun` 转发给 `runtime.Invoke` |

两个版本接真实 DeepSeek 跑过相同的三句输入（正常查单、查不存在的订单、诱导直接退款），行为一致。

接入 Eino 时踩到、并用测试证实的五个点：

1. **绝不能把 handler 直接注册成 Eino 工具。** 模型一调就真的执行了，治理层被整个绕过。
   正确做法是适配器：`InvokableRun` 里只转发给 `runtime.Invoke`，自己不含任何业务逻辑。
2. **治理结论要作为工具结果返回，不能返回 error。** CONFIRM、DENY 是正常结论，模型要读到
   `APPROVAL_REQUIRED` 这些错误码；返回 error 会让 Eino 把它当执行失败，整个 Agent 报错结束。
   `tool_call_id` 不在 `InvokableRun` 的参数里，要用 `compose.GetToolCallID(ctx)` 取。
3. **越权调用要配 `UnknownToolsHandler`。** 模型调用未注册的工具时，Eino 默认直接报错中断。
   把它也转给 `runtime.Invoke`，由执行期白名单返回 `TOOL_NOT_ALLOWED`，和手写版行为一致。
4. **流式模式要自定义 `StreamToolCallChecker`。** Eino 默认只看第一个非空 chunk 判断模型有没有调工具。
   DeepSeek 真实输出过"先说一句话再调工具"，默认实现会把开场白当最终答案、工具一次不执行——
   `TestEinoDefaultStreamCheckerMissesToolCallsAfterText` 把这个现象跑了出来。
5. **ToolsNode 默认并发执行同一轮的多个工具。** 手写版是串行的。适配器打印 `[tool_result]` 的 writer
   一开始没加锁，`-race` 当场报了数据竞争；治理框架本身没问题，因为 runtime、账本、审批库、审计口早就加了锁。

另外，`MaxStep` 数的是图节点的执行步数而不是对话轮数：ReAct 一轮 = 模型 1 步 + 工具 1 步，
所以设 16 才对应手写版的 8 轮，`TestEinoAgentStopsAfterMaxRounds` 验证了正好请求模型 8 次。

## 其他值得一提的小差异

| 主题 | Python | Go |
|---|---|---|
| 不可变上下文 | `@dataclass(frozen=True)` | 按值传递；但内部的 map 仍是共享的，靠约定只读 |
| 局部覆盖字段 | `dataclasses.replace(ctx, mode=...)` | `ctx.WithMode(...)`，值接收者改副本再返回 |
| 错误传播 | `raise PolicyDenied(code, msg)` | 返回 `*PolicyError`，用 `errors.As` 取错误码 |
| 参数容器 | `dict` | `json.RawMessage`，更贴近模型真实返回的 tool_calls |
| 反射取字段 | `getattr(args, "command", "")` | 类型断言 `args.(*RunShellArgs)` |
| 枚举 | `StrEnum` | 具名字符串类型 + 常量组 |
| 集合 | `frozenset` | `map[string]struct{}` 包一层 `StringSet` |

## 对照阅读

Python 版的阅读指南同样适用于这份代码：两边的章节划分、函数命名和注释都是一一对应的。
见 [../docs/reading_guide.md](../docs/reading_guide.md)。
