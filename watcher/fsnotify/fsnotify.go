// Package fsnotify 提供基于 github.com/fsnotify/fsnotify 的变化监听器。
//
// 为什么需要它：轮询（watcher/poll）最坏滞后一个周期，而事件式监听能在毫秒级
// 感知变化，适合对切换延迟敏感的进程。
//
// 依赖成本：本包引入 github.com/fsnotify/fsnotify（BSD-3），它只额外依赖
// golang.org/x/sys。两者都是事实标准库，无更深传递依赖；核心包 confwatch 不含
// 任何第三方依赖，不使用本包就不会进入构建。
//
// 使用要点（都是文件监听本身的坑，不是实现瑕疵）：
//   - 必须监听父目录而不是文件本身：原子替换（写临时文件 + rename）会换掉 inode，
//     盯着旧 inode 的 watcher 从此收不到任何事件。
//   - 一次修改可能产生多个事件（Write/Chmod/Create），编辑器也可能先写临时文件，
//     因此这里用定时器做去抖，把一段密集事件合并成一次 notify。
//   - 文件删除后重建同样会触发，Manager 每次都会重新读取内容，不依赖事件类型。
//   - 事件可能丢失（内核队列溢出）。每次触发都会重新读取并比对摘要，因此丢失
//     事件不会造成错乱，只会漏掉某次更新；需要更强保证时改用 watcher/poll。
package fsnotify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce 是未设置 Debounce 时使用的去抖窗口。
const DefaultDebounce = 50 * time.Millisecond

// Watcher 监听单个文件的父目录，并把针对该文件的密集事件合并为一次通知。
// 零值不可用，用 New 构造。
//
// 字段（Path/Debounce）必须在 Run 启动前设置好；启动后修改字段属于数据竞争，
// Run 内部不会重新读取它们。
type Watcher struct {
	// Path 是要观察的文件路径。必填。
	Path string
	// Debounce 是去抖窗口，<= 0 时使用 DefaultDebounce。
	Debounce time.Duration
}

// New 构造一个监听 path 的 watcher。
func New(path string) *Watcher {
	return &Watcher{Path: path, Debounce: DefaultDebounce}
}

// Run 实现 confwatch.Watcher：阻塞直到 ctx 结束或出现不可恢复错误。
//
// 返回 nil 表示按 ctx 正常停止；返回错误表示监听器已失效（例如目录被删除、
// 内核队列溢出），调用方应记录并考虑重建 Watcher。
func (w *Watcher) Run(ctx context.Context, notify func()) error {
	if w.Path == "" {
		return errors.New("fsnotify: empty path")
	}
	debounce := w.Debounce
	if debounce <= 0 {
		debounce = DefaultDebounce
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer fw.Close()

	// 监听父目录：原子替换会换掉 inode，只盯文件本身会漏掉后续所有变化。
	dir := filepath.Dir(w.Path)
	if err := fw.Add(dir); err != nil {
		return fmt.Errorf("fsnotify: watch %s: %w", dir, err)
	}

	target := filepath.Clean(w.Path)
	// 注册完成后立即对齐一次：从 Open 的首次读取到本监听器注册之间，文件可能已经
	// 变化，而那段时间内没有任何事件会到达。多触发一次无害：Manager 用载荷摘要
	// 判幂等，内容未变时不会发布。
	notify()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	// 定时器停止后其通道不会再就绪，因此 nil 表示"当前没有待触发的通知"。
	var fire <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-fw.Events:
			if !ok {
				return errors.New("fsnotify: events channel closed")
			}
			if !match(ev, target) {
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounce)
			fire = timer.C

		case <-fire:
			fire = nil
			notify()

		case err, ok := <-fw.Errors:
			if !ok {
				return errors.New("fsnotify: errors channel closed")
			}
			return fmt.Errorf("fsnotify: %w", err)
		}
	}
}

// match 判断事件是否与目标文件相关。
//
// 只关心内容可能变化的操作：Write/Create/Remove/Rename。纯 Chmod 不改变内容，
// 但某些编辑器用 chmod 作为"写完了"的信号，所以只要 Op 里带任何变化位就放行。
func match(ev fsnotify.Event, target string) bool {
	if filepath.Clean(ev.Name) != target {
		return false
	}
	return ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
}

// Describe 返回监听目标的描述，便于日志。
func (w *Watcher) Describe() string {
	return fmt.Sprintf("fsnotify:%s@%s", w.Path, w.Debounce)
}
