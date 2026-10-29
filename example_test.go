package confwatch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
	"github.com/brianbrian666/confwatch/decode/yaml"
	"github.com/brianbrian666/confwatch/source/file"
	"github.com/brianbrian666/confwatch/watcher/poll"
)

// Config 是业务配置。YAML 与 JSON 共用同一套 json 标签。
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

// Example 是 README 中的最小用法，同时也是它的可执行校验。
func Example() {
	ctx := context.Background()

	m, err := confwatch.Open[Config](ctx,
		confwatch.WithName("demo"),
		confwatch.WithSource(file.New("testdata/config.yaml")),
		confwatch.WithDecoder(confwatch.FormatYAML, yaml.New()),
		confwatch.WithValidator(confwatch.ValidatorFunc[Config](func(_ context.Context, c *Config) error {
			if c.Server.Addr == "" {
				return errors.New("server.addr is required")
			}
			return nil
		})),
	)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer func() { _ = m.Close() }()

	snap := m.Get()
	fmt.Printf("version=%d addr=%s max_open=%d\n", snap.Version(), snap.Value().Server.Addr, snap.Value().DB.MaxOpen)

	// 内容没变时 Reload 不会发布新版本：摘要相同即视为无变化。
	if _, err := m.Reload(ctx); err != nil {
		fmt.Println("reload:", err)
		return
	}
	fmt.Printf("version=%d\n", m.Get().Version())

	// Output:
	// version=1 addr=:8080 max_open=20
	// version=1
}

// ExampleManager_Watch 是 README 中文件热更新的可执行校验。
func ExampleManager_Watch() {
	dir, err := os.MkdirTemp("", "confwatch-example")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"addr":":8080"}}`), 0o600); err != nil {
		fmt.Println("write:", err)
		return
	}

	// 轮询监听零第三方依赖；对切换延迟敏感时换成 watcher/fsnotify。
	watcher := poll.New(path)
	watcher.Interval = 5 * time.Millisecond

	m, err := confwatch.Open[Config](context.Background(),
		confwatch.WithName("demo"),
		confwatch.WithSource(file.New(path)),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		confwatch.WithWatcher(watcher),
	)
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// Watch 阻塞在独立 goroutine 中，直到 ctx 取消或 Close。
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	// 模拟编辑器保存：写临时文件再原子替换。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"server":{"addr":":9090"}}`), 0o600); err != nil {
		fmt.Println("write:", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Println("rename:", err)
		return
	}

	// 实际程序直接 Get() 即可；示例里等新版本发布，让输出确定。
	deadline := time.Now().Add(2 * time.Second)
	for m.Get().Version() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	snap := m.Get()
	fmt.Printf("version=%d addr=%s\n", snap.Version(), snap.Value().Server.Addr)

	// 优雅停止：先取消 ctx 等 Watch 返回，再关闭 Manager。
	stop()
	if err := <-done; err != nil {
		fmt.Println("watch:", err)
		return
	}
	if err := m.Close(); err != nil {
		fmt.Println("close:", err)
		return
	}
	fmt.Println("stopped")

	// Output:
	// version=2 addr=:9090
	// stopped
}
