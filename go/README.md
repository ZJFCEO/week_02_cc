# Go 版：工具治理与权限状态机

`tool_governance_demo.py` 的 Go 复刻，包含第二章作业的转账工具和五个链路测试。

行为与 Python 版对齐：同样的九次演示调用、同样的错误码、同样的脱敏结果、同样的审计条数。
但有三处语言层面的真实差异，注释里都标了 `【Go 差异】`，那才是这份复刻值得读的地方。

## 快速开始

```bash
go run ./cmd/demo
```

```bash
go test ./governance/ -v -run Transfer
```

跑全部测试（含一个专门演示 Go 超时语义的用例）：

```bash
go test ./... -race -v
```

## 包结构

```
go/
├── cmd/demo/main.go                  离线演示入口
└── governance/
    ├── types.go                      枚举、执行上下文、工具策略、工具定义、结果与审计记录
    ├── errors.go                     PolicyError / TransientError / ValidationError
    ├── args.go                       Args 接口、四个参数结构体、DecodeArgs（extra=forbid 的等价物）
    ├── approval.go                   参数摘要、ApprovalStore（一次性 + 参数绑定）
    ├── audit.go                      AuditSink、危险命令黑名单、Redact 脱敏
    ├── engine.go                     PermissionEngine.Decide —— 九步固定优先级
    ├── runtime.go                    ToolRuntime.Invoke —— 一次调用的四个阶段
    ├── tools.go                      模拟数据、四个工具实现、注册与运行时组装
    ├── demo.go                       九次演示调用
    ├── transfer_test.go              作业要求的五个测试
    └── timeout_semantics_test.go     证明 Go 的超时不等于取消
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
