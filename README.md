# confwatch

一个 Go 配置热更新库：**原子发布、不可变快照、加载失败不影响当前有效配置**。

> 当前状态：第一阶段骨架（v0.1-dev）。已实现快照管理、配置源/解码器/校验器接口、错误分类、
> 按需 Reload。**尚未实现**：文件变化监听、环境变量覆盖、多层合并、变更通知、回滚。

## 安装

```bash
go get github.com/brianbrian666/confwatch
```

需要 Go 1.24+（使用泛型与 `atomic.Pointer[T]`）。

## 最小示例

`config/app.yaml`：

```yaml
server:
  addr: ":8080"
  read_timeout_seconds: 5
db:
  dsn: "postgres://localhost:5432/app?sslmode=disable"
  max_open: 20
```

`main.go`：

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/brianbrian666/confwatch"
	"github.com/brianbrian666/confwatch/decode/yaml"
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
		confwatch.WithSource(file.New("config/app.yaml")),
		confwatch.WithDecoder(confwatch.FormatYAML, yaml.New()),
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

	// 读路径：无锁、不阻塞、不分配。
	cfg := m.Get().Value()
	fmt.Println(cfg.Server.Addr, cfg.DB.MaxOpen)

	// 触发一次更新（后续由文件监听自动触发，走的是同一条流水线）。
	if _, err := m.Reload(ctx); err != nil {
		log.Printf("reload failed, keeping version %d: %v", m.Get().Version(), err)
	}
}
```

YAML 与 JSON 共用同一套 `json` 标签；JSON 配置把 `yaml.New()` 换成
`json.New()`（`github.com/brianbrian666/confwatch/decode/json`）即可。

## 它保证什么

| 保证 | 说明 |
| --- | --- |
| 原子发布 | 读者要么看到旧快照，要么看到新快照；版本号、摘要、配置值同属一个对象，不会撕裂 |
| 读路径无锁 | `Get()` = 一次 `atomic.Pointer.Load` + 一次结构体拷贝，不阻塞、不分配 |
| 失败不生效 | 读取/解码/校验任一步失败，当前快照逐字节不变，只更新 `LastError()` |
| 版本单调 | 版本号在写者临界区内分配，只在成功发布时 +1，不重复、不覆盖 |
| 旧快照不变 | 每次加载都解码出全新对象，已发布快照在库内部永不被写 |
| 内容幂等 | 摘要相同则不发布、不递增版本、不触发后续通知 |

## 它不保证什么

- **`T` 的深不可变**。`Snapshot.Value()` 返回 `T` 的浅拷贝：值类型字段安全，`map`/`slice`/指针
  字段与快照共享底层数据。修改它们属于未定义行为。需要彻底隔离时用 `WithCopyFunc` 在发布前深拷贝。
- **外部资源的一致性**。「发布新快照」不等于「连接池已重建、端口已换绑」。资源重建属于上层职责。
- **零值配置的合法性**。空文档 / `null` 文档会被解码器直接判为错误，避免静默发布一份零值配置。

## 并发模型

```
Reload/TryReload ──► 写者令牌(容量 1 的 channel，可被 ctx 取消)
                       │
                       ├─ 读取源 → 解码 → 拷贝 → 默认值 → 校验 → 构造快照
                       │                              （失败：不触碰当前快照）
                       └─ atomic.Pointer.Store  ← 唯一生效点

Get ──► atomic.Pointer.Load ──► Snapshot[T]（无锁）
```

- `atomic.Pointer` 的 `Store`/`Load` 具有 release/acquire 语义：`Store` 之前的全部写入，对之后
  观察到该指针的 `Load` 都可见，因此不需要额外加锁或屏障。
- 这套保证成立的前提是**发布后不再写入该对象**，所以库内部绝不复用上一份快照的缓冲区。
- `TryReload` 在已有发布进行中时立即返回 `ErrReloadInFlight`，适合定时器或事件循环调用。

## 错误分类

用 `errors.Is` 判断类别，用 `errors.As` 取出 `*confwatch.Error` 查看诊断字段
（`Op`/`Name`/`Source`/`Version`/`Attempt`）。

| 哨兵错误 | 含义 |
| --- | --- |
| `ErrNotConfigured` | 选项不完整或不合法（缺源、缺解码器、nil 选项、nil context） |
| `ErrNoDecoder` | 载荷格式没有注册解码器 |
| `ErrSource` | 配置源读取失败 |
| `ErrDecode` | 解析失败（语法、类型、空文档、`null` 文档） |
| `ErrValidation` | 校验失败，多个校验器的错误被 `errors.Join` 聚合 |
| `ErrNotInitialized` | 在零值 `Manager` 上调用了写入方法 |
| `ErrCanceled` | 加载被 context 取消，同时包装 `context.Canceled`/`DeadlineExceeded` |
| `ErrClosed` | Manager 已关闭 |
| `ErrReloadInFlight` | `TryReload` 时已有发布在进行 |

没有「发布失败」这一类：发布的唯一动作是 `atomic.Pointer.Store`，它不会失败。

## 目录结构

```
confwatch/
├── doc.go              # 包文档：原子发布/不可变/快照读取/调用方引用 四条边界
├── snapshot.go         # Snapshot[T]、Version、Reason
├── manager.go          # Open/Get/Reload/TryReload/Close/LastError/Stats + 发布流水线
├── options.go          # Option 与 With* 选项
├── source.go           # Source、Payload、Format、Decoder 接口
├── validator.go        # Validator[T]、ValidatorFunc[T]
├── errors.go           # 哨兵错误与 *Error
├── decode/json         # JSON 解码器（仅 stdlib）
├── decode/yaml         # YAML 解码器（yaml.v3 -> JSON 数据模型）
├── source/file         # 本地文件源（按扩展名推断格式）
└── testdata/           # 示例与测试夹具
```

## 开发

```bash
go test ./...
go test -race ./...
go vet ./...
```

## 路线图

1. ~~快照、原子发布、错误分类~~（已完成）
2. 多层覆盖（Overlay + 环境变量）、深合并语义、复合摘要
3. 文件变化监听（fsnotify + 轮询兜底、去抖、摘要判据）
4. 变更通知（每订阅有界队列、丢弃计数、`Flush`）
5. 历史快照与回滚（回滚产生新版本号，不复活旧号）
6. 基准测试与 `benchstat` 回归门禁
