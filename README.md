# confwatch

Go 配置热更新库：**原子发布、不可变快照、加载失败不影响当前有效配置**。

```go
cfg := m.Get().Value()   // 无锁、不分配、永远是一份完整的配置
```

> **状态：v0.2（开发中，未发布正式版本）**。已实现：快照与原子发布、配置源/解码器/校验器接口、
> 错误分类、手动 `Reload`、文件变化监听（轮询 + fsnotify）、并发与泄漏测试、基准测试。
> **尚未实现**：变更通知（订阅回调）、多层覆盖（环境变量/默认值文件）、历史回滚、
> 远程配置中心。详见[已知限制](#已知限制)与[未来计划](#未来计划)。
>
> 本项目**尚未在任何生产环境使用**，README 中不含用户案例。API 在 v1.0 之前可能发生不兼容变更，
> 每次变更都记入 [CHANGELOG](CHANGELOG.md)。

---

## 1. 项目介绍

`confwatch` 把「一份配置」建模为一个**不可变快照**：加载、解析、校验全部在写者侧完成后，
用一次 `atomic.Pointer.Store` 让它对读者一次性、原子地生效。读者只做一次原子加载加一次结构体拷贝，
不阻塞、不加锁、不与写者共享任何可变状态。

它解决的是配置热更新中最容易写错的那部分：并发、失败隔离、生命周期。它**不**负责的事情同样明确：
不重建连接池、不重启服务、不做多层配置合并、不做配置中心客户端。

最小代码量：

```go
m, err := confwatch.Open[Config](ctx,
	confwatch.WithSource(file.New("config/app.yaml")),
	confwatch.WithDecoder(confwatch.FormatYAML, yamlcodec.New()),
)
cfg := m.Get().Value()
```

## 2. 为什么需要这个项目

常见的热更新写法有几种典型问题，`confwatch` 针对性地给出了答案：

| 常见写法 | 问题 | confwatch 的做法 |
| --- | --- | --- |
| `var C Config` + `json.Unmarshal` 到全局变量 | 更新过程中读者可能读到半新半旧的字段；并发读写是数据竞争 | 解到新对象，校验通过后一次性 `Store` 指针；读者永远看到完整对象 |
| `sync.RWMutex` 保护配置 | 读路径有锁，写者持锁期间读者排队；忘记加锁无法被编译器发现 | 读路径只有 `atomic.Pointer.Load`，不加锁 |
| 加载失败直接覆盖 | 一次写坏的配置文件让服务带着零值配置继续跑 | 任一步失败都不触碰当前快照，旧配置继续生效，错误写入 `LastError()` |
| 在 `time.Tick` 里 `go reload()` | 事件处理 goroutine 无界堆积；停止时不收敛，退出时泄漏 | 容量 1 的合并信箱 + 阻塞式 `Watch`，停止前 join 监听 goroutine |
| 只监听一个 inode | 编辑器「临时文件 + rename」后监听失效，再也不更新 | 监听父目录（fsnotify）或比对内容摘要（poll） |

如果你只需要「启动时读一次配置」，标准库 `os.ReadFile` + `json.Unmarshal` 就够了，不需要本库。

## 3. 安装

```bash
go get github.com/brianbrian666/confwatch
```

```bash
# 可选：YAML 解码器（引入 gopkg.in/yaml.v3）
go get github.com/brianbrian666/confwatch/decode/yaml

# 可选：事件式监听器（引入 github.com/fsnotify/fsnotify 及其唯一传递依赖 golang.org/x/sys）
go get github.com/brianbrian666/confwatch/watcher/fsnotify
```

依赖边界（未列出的包只用标准库）：

| 包 | 第三方依赖 |
| --- | --- |
| `confwatch`、`decode/json`、`source/file`、`watcher/poll` | 无 |
| `decode/yaml` | `gopkg.in/yaml.v3` |
| `watcher/fsnotify` | `github.com/fsnotify/fsnotify`（→ `golang.org/x/sys`，indirect） |

不使用某个包，它就不会进入你的构建图。核心包刻意不引入任何第三方依赖，这是本项目的硬约束。

### Go 版本兼容性

- `go.mod` 声明的最低版本是 **Go 1.24**（依赖泛型方法外推、`atomic.Pointer[T]`、`errors.Join`、
  `context.AfterFunc`）。
- 本次提交的验证环境是 **go1.27.3 darwin/arm64**：`go test`、`go test -race`、`go vet`、
  `go test -bench` 全部通过。
- CI 矩阵覆盖 `1.24.x`（声明的最低版本）与 `1.27.x`（当前开发版本）；只有 CI 实际跑通的版本
  才会被写进上表。未安装 Go 1.24 的本地环境无法验证最低版本，这一点不做承诺。
- 不使用 `go:build` 约束，也不使用 `//go:linkname` 之类会随版本失效的机制。

## 4. 最小示例

`config/app.json`：

```json
{
  "server": { "addr": ":8080", "read_timeout_seconds": 5 },
  "db": { "dsn": "postgres://localhost:5432/app?sslmode=disable", "max_open": 20 }
}
```

```go
package main

import (
	"context"
	"errors"
	"log"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
	"github.com/brianbrian666/confwatch/source/file"
)

type Config struct {
	Server struct {
		Addr               string `json:"addr"`
		ReadTimeoutSeconds int    `json:"read_timeout_seconds"`
	} `json:"server"`
	DB struct {
		DSN     string `json:"dsn"`
		MaxOpen int    `json:"max_open"`
	} `json:"db"`
}

func main() {
	ctx := context.Background()

	m, err := confwatch.Open[Config](ctx,
		confwatch.WithName("api"),
		confwatch.WithSource(file.New("config/app.json")),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		confwatch.WithValidator(confwatch.ValidatorFunc[Config](func(_ context.Context, c *Config) error {
			if c.Server.Addr == "" {
				return errors.New("server.addr is required")
			}
			return nil
		})),
	)
	if err != nil {
		log.Fatalf("open config: %v", err) // 错误里带 source/version/attempt
	}
	defer m.Close()

	cfg := m.Get().Value() // 读路径：无锁、不阻塞、不分配
	log.Println(cfg.Server.Addr, cfg.DB.MaxOpen)
}
```

可执行示例见 `example_test.go`（`ExampleManager_Watch` 会真正启动监听、改名替换文件、观察版本变化），
以及 [`examples/watchfile`](examples/watchfile/main.go)（可直接 `go run` 的文件监听 demo，零第三方依赖）。

YAML 与 JSON 共用同一套 `json` 标签：把 `jsoncodec.New()` 换成 `yamlcodec.New()`、
`FormatJSON` 换成 `FormatYAML` 即可。

## 5. 配置加载方式

```go
type Source interface {
	Load(ctx context.Context) (Payload, error)
	Describe() string
}
```

内置 `source/file`：读文件、按扩展名推断格式（`.json` / `.yaml` / `.yml`），
也可用 `file.NewWithFormat(path, confwatch.FormatYAML)` 显式指定。

自定义源只需实现两个方法（例如从环境变量、K8s Secret 卷、共享内存读取）：

```go
type envSource struct{ key string }

func (s envSource) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}
	v, ok := os.LookupEnv(s.key)
	if !ok {
		return confwatch.Payload{}, fmt.Errorf("environment variable %s is not set", s.key)
	}
	return confwatch.Payload{Data: []byte(v), Format: confwatch.FormatJSON}, nil
}

func (s envSource) Describe() string { return "env:" + s.key }
```

源实现的约束（写在 `Source` 文档里，违反会导致难以定位的问题）：

- **必须尊重 `ctx`**：写者令牌与 `Reload` 的取消都依赖它，忽略 ctx 会让 `Reload` 和 `Close` 挂住。
- **每次返回的 `Data` 不得被后续调用复用**：库不会替你拷贝整个字节块（只在校验前深拷贝 `T`）。
- **`Payload.Digest` 只在你能比 `sha256(Data)` 更准确判断"内容是否相同"时才填**（例如上游 revision）。
  返回常量或滞后摘要会让真实更新被静默跳过。

解码器是一个只有一个方法的接口：

```go
type Decoder interface {
	Decode(data []byte, dst any) error
}
```

`decode/json` 用 `json.Unmarshal`，并对空文档、`null` 文档显式报错（避免静默发布零值配置）；
`decode/yaml` 先把 YAML 转成 JSON 数据模型再复用同一套 `json` 标签。自定义解码器（TOML、INI、
protobuf）只要满足「每次产生全新对象、失败不留半成品」即可接入。

## 6. 配置更新机制

两条触发路径，**同一条流水线**：

```
手动：m.Reload(ctx) / m.TryReload(ctx)
监听：Watcher.Run ──notify──► 容量 1 的合并信箱 ─────────┐
                                                        ▼
写者令牌（容量 1 的 channel，可被 ctx 取消）
   └─► 读取源 → 解码 → 深拷贝 → 默认值 → 全部校验 → 构造快照 → 摘要比对
                                                                  │
                                            内容变了吗？ ── 否 ──► 跳过（版本不变）
                                                  │ 是
                                                  ▼
                                     atomic.Pointer.Store（唯一生效点）
```

关键语义：

- **版本号**只在写者临界区内分配，仅成功发布时递增；内容相同（摘要一致）不发布、不递增。
- **`TryReload`** 在已有发布进行时立刻返回 `ErrReloadInFlight`，适合定时器/事件循环调用；
  `Reload` 会排队等待（受 ctx 控制）。
- **监听器不会被配置错误拖死**：文件内容坏了只写 `LastError()` 并调用 `ErrorHook`，监听继续，
  文件修好后自动恢复。
- **合并而非排队**：监听触发先进容量 1 的信箱，满则丢弃；一次慢重载期间连续 100 次写入最终
  只多触发一次重载，而那次重载读的是最新内容。库保证收敛，不保证"每次写入都触发一次"。

```go
ctx, stop := context.WithCancel(context.Background())
defer stop()

done := make(chan error, 1)
go func() { done <- m.Watch(ctx) }()

// ... 业务运行，直接 m.Get().Value() ...

stop()                    // 或 m.Close()，两者都可重复调用
if err := <-done; err != nil { /* ErrWatcher：监听器自身故障 */ }
```

### 两种监听器怎么选

| | `watcher/poll` | `watcher/fsnotify` |
| --- | --- | --- |
| 依赖 | 无（仅 stdlib） | `fsnotify` + `golang.org/x/sys` |
| 延迟 | 最坏一个轮询周期（默认 1s，可配） | 毫秒级（去抖默认 50ms） |
| 原子替换 / 删除重建 | 天然支持（比对内容摘要） | 支持（监听父目录，去抖合并） |
| 事件丢失 | 无此问题 | 可能丢事件，靠每次重新读取兜底 |
| K8s ConfigMap（切换 `..data` 符号链接） | 支持 | **不匹配**（只认目标文件名，请用 poll） |
| 适用 | 单文件、NFS、符号链接切换、无法引入依赖 | 本地文件、对切换延迟敏感 |

两者都会在注册完成后立即「对齐」一次，覆盖 `Open` 读文件与监听建立之间的窗口；多触发的部分
由摘要判幂等吸收，不会产生多余版本。

## 7. 校验与错误处理

校验器在**发布之前**运行，多个校验器的错误用 `errors.Join` 聚合，任一失败则不发布：

```go
confwatch.WithValidator(confwatch.ValidatorFunc[Config](func(ctx context.Context, c *Config) error {
	var errs []error
	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is required"))
	}
	if c.DB.MaxOpen <= 0 {
		errs = append(errs, fmt.Errorf("db.max_open must be > 0, got %d", c.DB.MaxOpen))
	}
	return errors.Join(errs...)   // nil 表示通过
}))
```

错误用 `errors.Is` 判类别，用 `errors.As` 取 `*confwatch.Error` 看诊断字段
（`Op` / `Name` / `Source` / `Version` / `Attempt` / `Cause`）：

| 哨兵错误 | 含义 | 当前快照 |
| --- | --- | --- |
| `ErrNotConfigured` | 选项不完整或不合法（缺源、缺解码器、nil 选项、nil context） | 不变 |
| `ErrNoDecoder` | 载荷格式没有注册解码器 | 不变 |
| `ErrSource` | 配置源读取失败（文件不存在、权限不足……） | 不变 |
| `ErrDecode` | 解析失败（语法、类型、空文档、`null` 文档） | 不变 |
| `ErrValidation` | 校验失败 | 不变 |
| `ErrNotInitialized` | 在零值 `Manager` 上调用写入方法 | — |
| `ErrCanceled` | 被 ctx 取消（同时包装 `context.Canceled`/`DeadlineExceeded`） | 不变 |
| `ErrClosed` | `Manager` 已关闭 | 保持最后一份 |
| `ErrReloadInFlight` | `TryReload` 撞上正在进行中的发布 | 保持旧值 |
| `ErrWatcher` | 监听器自身不可恢复故障（含 panic、未报错就退出） | 不变 |
| `ErrAlreadyWatching` | 同一 `Manager` 上已有 `Watch` | 不变 |

没有「发布失败」这一类：发布的唯一动作是 `atomic.Pointer.Store`，它不会失败。

后台（监听路径）失败如何被观察到——因为调用方拿不到返回值：

```go
confwatch.WithErrorHook(func(err error) { log.Printf("config reload failed: %v", err) }),
// 以及随时可读：
m.LastError()
```

`ErrorHook` 只在调用方无法收到错误的地方触发（监听触发的重载失败、监听器致命故障），同步执行、
panic 会被 recover；耗时逻辑请自行丢进队列，否则会拖慢其后的事件处理。

## 8. 并发安全说明

**读者与写者的契约**（这是本库最重要的行为，也是唯一需要读者配合的地方）：

1. **原子发布**：`Store`/`Load` 具有 release/acquire 语义，`Store` 之前的全部写入对之后观察到
   该指针的 `Load` 都可见。因此不需要额外加锁或内存屏障。
2. **对象不可变**：库内部在发布后绝不再写该对象，也绝不复用上一份快照的缓冲区（解码必须产出新对象；
   `WithCopyFunc` 会在校验前深拷贝一次 `T`）。
3. **快照读取**：`Get()` 返回 `Snapshot[T]` 值，`Value()` 返回 `T` 的**浅拷贝**。版本号、摘要、
   配置值属于同一个对象，读者不会读到「新版本号 + 旧配置」这种撕裂组合。
4. **调用方持有的引用**：`map`/`slice`/指针字段与快照共享底层数据。**读者不得修改它们**——这属于
   未定义行为，Go 无法在类型层面阻止。需要彻底隔离就用 `WithCopyFunc` 在发布前深拷贝，
   或者接受「只读约定」。

其它并发事实：

- **无版本覆盖**：所有写入都先拿写者令牌（容量 1 的 channel，可被 ctx 取消），全程串行；
  版本号只在临界区内分配，因此发布顺序与版本号一致。锁序固定为「令牌 → `mu`（仅用于计数器与
  `closed` 标志）」，不存在反向获取，故不会死锁。
- **读路径不加锁**：`Get()` 不接触任何 mutex，写者持有令牌期间读者照常服务。
- **`Watch` 与 `Close` 并发**：`Close` 取消内部 `baseCtx`，`Watch` 通过 `context.AfterFunc` 连接到它，
  在 `runCtx` 结束时 join 监听 goroutine 后才返回；`Watch` 返回后不再有任何自动重载。
- **不会泄漏 goroutine**：`Watch` 返回前一定等监听器 goroutine 结束（监听器 panic 或静默退出也会
  被归约为 `ErrWatcher`）；`TestWatchDoesNotLeakGoroutines` 用真实监听器跑 20 轮启停，断言
  goroutine 数回到基线。
- **无无界队列**：唯一的队列是容量 1 的合并信箱（满了丢弃）。计数器是固定几个整数，不随事件增长。
- **无订阅者**：本项目当前**没有**变更通知 API，因此不存在「订阅者队列积压」「订阅者退出后仍收到
  通知」「慢订阅者拖住写者」这类问题——它们会在实现通知时一并设计（见未来计划）。
- `atomic.Pointer` 只能保护指针本身；`Manager` 内部还有 `sync.Mutex` 与 `atomic.Bool`，因此
  **`Manager` 不可拷贝**（`go vet` 的 copylocks 检查会拦住）。用 `*Manager[T]` 传递。

## 9. API 文档入口

- pkg.go.dev：<https://pkg.go.dev/github.com/brianbrian666/confwatch>（首个 tag 推送后生效）
- 本地：`go doc -all github.com/brianbrian666/confwatch`
- 包内文档：`doc.go`（四条不可变边界、并发约定、Watch 语义），各类型的文档注释均包含"违反契约会发生什么"。

公开 API 一览（v0.2）：

```go
func Open[T any](ctx context.Context, opts ...Option) (*Manager[T], error)
func (m *Manager[T]) Get() Snapshot[T]
func (m *Manager[T]) Reload(ctx context.Context) (Snapshot[T], error)
func (m *Manager[T]) TryReload(ctx context.Context) (Snapshot[T], error)
func (m *Manager[T]) Watch(ctx context.Context) error
func (m *Manager[T]) LastError() error
func (m *Manager[T]) Stats() Stats
func (m *Manager[T]) Close() error

type Watcher interface{ Run(ctx context.Context, notify func()) error }
```

## 10. 性能测试方法

```bash
go test -bench=. -benchmem ./...
go test -bench=BenchmarkGet -benchmem -count=10 ./... | tee new.txt
benchstat old.txt new.txt     # 做回归对比（需 go install golang.org/x/perf/cmd/benchstat）
```

现有基准（`bench_test.go`）：

| 基准 | 测什么 |
| --- | --- |
| `BenchmarkGet` | 原子加载 + 快照结构体拷贝 |
| `BenchmarkGetValue` | 取出配置值（与上者的差值 = `T` 的拷贝成本） |
| `BenchmarkGetParallel` | 多 P 并发读，验证读路径无隐藏争用 |
| `BenchmarkReloadNoop` | 内容未变时的完整流水线（读取+解码+校验，不发布） |
| `BenchmarkReloadPublish` | 内容每次变化（与上者同形状文档，可直接相减） |
| `BenchmarkReloadParallel` | 并发 Reload 排队与令牌交接成本 |

本机实测（**darwin/arm64, Apple M5, go1.27.3**，10 个逻辑核，`-benchmem` 单次运行，仅作量级参考，
不是性能承诺）：

```
BenchmarkGet-10               225480201     5.158 ns/op     0 B/op    0 allocs/op
BenchmarkGetValue-10          216492194     5.552 ns/op     0 B/op    0 allocs/op
BenchmarkGetParallel-10      1000000000     0.8175 ns/op    0 B/op    0 allocs/op
BenchmarkReloadNoop-10          1433710     830.7 ns/op   1193 B/op   17 allocs/op
BenchmarkReloadPublish-10       1353594     889.2 ns/op   1369 B/op   21 allocs/op
BenchmarkReloadParallel-10      1226572     982.0 ns/op    890 B/op   13 allocs/op
```

读路径零分配不是文档口号，而是断言：`TestGetDoesNotAllocate` 用 `testing.AllocsPerRun` 检查
`Get()` / `Get().Value()` / `Stats()` 每次调用分配 0 次。

这些数字**不含**文件系统读取、`fsnotify` 与去抖；真实端到端延迟还取决于磁盘与监听器周期。

## 11. 已知限制

按"影响面"排序，全部是已知且有意为之的取舍：

1. **`T` 的深不可变靠约定**。`Value()` 是浅拷贝，`map`/`slice`/指针字段与快照共享内存。
   读者改了就是未定义行为。彻底隔离要付深拷贝的成本（`WithCopyFunc` + 写者侧额外分配）。
2. **`fsnotify` 实现只认目标文件名**。K8s ConfigMap 的 `..data` 符号链接切换方式触发的是
   `..data`、`..2026_..` 之类名字，不会被匹配；这种情况用 `watcher/poll`。
3. **事件式监听可能丢事件**，且没有周期性兜底（不会自己发现"我漏了"）。
4. **非原子写入可能短暂发布一份"语法有效但语义不完整"的配置**。库只能保证"解析+校验失败不发布"；
   如果编辑器先截断再写，中间那一刻恰好是合法 JSON，它就会被发布。写盘请用临时文件 + rename。
5. **去抖与写入节奏可能打架**：fsnotify 默认 50ms 去抖，若某程序以 >20 次/秒持续写文件，
   可能在写入间隙发布中间状态。可调 `Debounce`。
6. **`Close` 不等待正在进行中的 `Reload` 完成**（它需要先拿到写者令牌）。一个忽略 ctx 的 `Source`
   会让 `Close` 无限期等待——这是 `Source` 契约的一部分，不是 bug。
7. **监听器忽略 ctx 会让 `Watch` 无法返回**。同上，属于 `Watcher` 契约。
8. **`ErrWatcher` 之后不自动重启**：库不会自己重建监听器，由调用方决定退避策略。
   `Watch` 可以重复调用（`TestWatchIsRestartable`）。
9. **只有单文件/单源**：没有多源合并、优先级、环境变量覆盖。
10. **没有变更通知**：更新后无法得到回调，只能轮询 `m.Get().Version()`。
11. **没有历史与回滚**：旧快照可以被调用方自己持有（它不可变），但库不保存历史。
12. **不限制配置体积**：读文件不设上限，超大文件会按解码器内存开销放大。配置文件应视为可信输入。

## 12. 未来计划

按依赖顺序，先做能解决真实痛点的，不排"好看但没人用"的功能：

1. **变更通知（下一步）**：`m.Subscribe()` 返回带界缓冲区 + 丢弃计数的通道，或 `OnChange(func(Change[T]))`；
   `Change` 携带 `Reason`/`Version`，慢订阅者丢中间版本但不阻塞写者；订阅取消后不再收到通知。
   这是"热更新闭环"缺的最后一块。
2. **混合监听器 `watcher/auto`**：fsnotify 为主 + 低频 poll 兜底，直接消掉限制 2、3（接口不变）。
3. **`benchstat` 回归门禁**：把基准接进 CI，防止读路径悄悄变慢。
4. **多层覆盖**：`Overlay` / 环境变量 / 默认值文件，复合摘要（解决"覆盖层变了但基座没变"）。
5. **历史与回滚**：`History(n)`、`Rollback(v)`（回滚产生新版本号，不复活旧号）。
6. **远程配置中心**：只做接口适配，不在核心包里引入网络依赖。

## 依赖升级策略

- **核心包零第三方依赖**是不可协商的约束；任何往根包加依赖的 PR 都会被拒绝。
- 第三方依赖只允许出现在可选子包（`decode/*`、`watcher/*`），且必须是该领域事实标准的库。
- 依赖升级走独立 PR：`go get -u <module> && go mod tidy`，附 `go test -race ./...` 结果，
  以及 `git diff go.mod`（新增传递依赖必须在 PR 描述里说明）。
- 安全更新（有 CVE/GO-202x 的依赖）例外，可随时合入并在 CHANGELOG 标 `Security`。
- Go 最低版本只在 CI 实际跑通后上调；上调属于 minor 变更，写入 CHANGELOG。

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，
不要开公开 issue。

## 许可

[MIT](LICENSE)。
