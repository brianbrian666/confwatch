// Command watchfile 演示 confwatch 的文件热更新：启动时加载配置，文件变化后自动
// 重载；解析或校验失败时继续使用上一份有效配置，只打印错误。
//
// 运行（在仓库根目录）：
//
//	go run ./examples/watchfile -config examples/watchfile/config.json -interval 200ms
//
// 然后在另一个终端改 examples/watchfile/config.json，观察输出的版本号变化。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
	"github.com/brianbrian666/confwatch/source/file"
	"github.com/brianbrian666/confwatch/watcher/poll"
)

type config struct {
	Server struct {
		Addr            string `json:"addr"`
		ReadTimeoutSecs int    `json:"read_timeout_seconds"`
	} `json:"server"`
	Debug bool `json:"debug"`
}

func main() {
	path := flag.String("config", "examples/watchfile/config.json", "配置文件路径")
	interval := flag.Duration("interval", 200*time.Millisecond, "轮询间隔（越短越及时，代价是更多磁盘读取）")
	flag.Parse()

	// 用信号控制生命周期：Ctrl-C 或 SIGTERM 都会触发优雅停止。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pw := poll.New(*path)
	pw.Interval = *interval

	m, err := confwatch.Open[config](ctx,
		confwatch.WithName("watchfile"),
		confwatch.WithSource(file.New(*path)),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		confwatch.WithWatcher(pw),
		confwatch.WithValidator(confwatch.ValidatorFunc[config](func(_ context.Context, c *config) error {
			if c.Server.Addr == "" {
				return errors.New("server.addr is required")
			}
			return nil
		})),
		// 监听触发的失败没有返回值可拿，只能通过 hook 或 LastError 观察。
		confwatch.WithErrorHook(func(err error) {
			log.Printf("重载失败，继续使用旧配置: %v", err)
		}),
	)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	defer func() { _ = m.Close() }()

	dump(m.Get())

	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	// 本库当前没有变更通知 API，所以这里轮询版本号来观察更新。写上通知后这段
	// 可以退化成订阅回调（见 README 的「未来计划」）。
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	last := m.Get().Version()
	for {
		select {
		case <-ctx.Done():
			if err := <-done; err != nil {
				log.Printf("监听器故障退出: %v", err)
			}
			log.Printf("退出，最后版本 v%d", m.Get().Version())
			return
		case <-ticker.C:
			if snap := m.Get(); snap.Version() != last {
				last = snap.Version()
				dump(snap)
			}
		}
	}
}

func dump(s confwatch.Snapshot[config]) {
	c := s.Value()
	fmt.Printf("v%d %s addr=%s timeout=%ds debug=%t\n",
		s.Version(), s.Reason(), c.Server.Addr, c.Server.ReadTimeoutSecs, c.Debug)
}
