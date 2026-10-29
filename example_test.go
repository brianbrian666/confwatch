package confwatch_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/brianbrian666/confwatch"
	"github.com/brianbrian666/confwatch/decode/yaml"
	"github.com/brianbrian666/confwatch/source/file"
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
