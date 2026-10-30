package fsnotify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.json")
	writeFile(t, path, body)
	return path
}

// running 是测试用的监听器句柄。
type running struct {
	ctx      context.Context
	cancel   context.CancelFunc
	notified chan struct{}
	done     chan error
}

// start 启动监听。真实文件系统事件无法精确预测，因此这里用可控的短去抖窗口，
// 并在断言处给足超时。
//
// 返回时监听器已完成注册：等待并收掉 Run 在注册后发出的启动对齐通知，
// 这就是测试需要的“已就绪”信号。
func start(t *testing.T, path string) *running {
	t.Helper()
	w := New(path)
	w.Debounce = 5 * time.Millisecond

	r := &running{notified: make(chan struct{}, 16), done: make(chan error, 1)}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	go func() { r.done <- w.Run(r.ctx, func() { r.notified <- struct{}{} }) }()

	r.waitNotify(t)
	r.drain()
	return r
}

func (r *running) waitNotify(t *testing.T) {
	t.Helper()
	select {
	case <-r.notified:
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内没有收到变化通知")
	}
}

func (r *running) drain() {
	for {
		select {
		case <-r.notified:
		default:
			return
		}
	}
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Run 在 ctx 取消后必须返回 nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

func TestRunNotifiesOnWrite(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	r := start(t, path)
	defer r.stop(t)

	writeFile(t, path, `{"addr":"b"}`)
	r.waitNotify(t)
}

// TestRunNotifiesOnAtomicReplace 覆盖"临时文件 + rename"的编辑器保存方式。
func TestRunNotifiesOnAtomicReplace(t *testing.T) {
	path := newFile(t, `{"addr":"old"}`)
	r := start(t, path)
	defer r.stop(t)

	tmp := filepath.Join(filepath.Dir(path), ".app.json.tmp")
	writeFile(t, tmp, `{"addr":"new"}`)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r.waitNotify(t)
}

func TestRunNotifiesOnDeleteAndRecreate(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	r := start(t, path)
	defer r.stop(t)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	r.waitNotify(t)
	r.drain()

	writeFile(t, path, `{"addr":"b"}`)
	r.waitNotify(t)
}

// TestRunFailsWhenWatchedDirGone 覆盖“父目录被删除”这一静默失效：fsnotify 会在目录
// inode 消失后把 watch 从内部表里摘掉，内核不再投递任何事件，而唯一能反映失效的
// 事件 Name 是目录本身，会被针对文件名的过滤丢掉。修复前实测：删除并重建目录后
// 再写文件，永不通知、永不报错、Run 也不返回。
func TestRunFailsWhenWatchedDirGone(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	r := start(t, path)
	defer r.cancel() // 这里期待 Run 以错误返回，因此不使用要求 nil 的 stop。

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatalf("remove dir: %v", err)
	}

	select {
	case err := <-r.done:
		if err == nil {
			t.Fatal("目录被删除后 Run 必须返回错误，否则监听静默失效")
		}
		if !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("Run 错误 = %v, want 包含 is gone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("目录被删除后 Run 未返回错误（监听静默失效）")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	r := start(t, path)

	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未返回")
	}
	// 重复取消是安全的。
	r.cancel()

	// 返回后不再通知。
	r.drain()
	writeFile(t, path, `{"addr":"b"}`)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-r.notified:
		t.Fatal("Run 返回后不应再通知")
	default:
	}
}

func TestRunRejectsEmptyPath(t *testing.T) {
	w := &Watcher{}
	if err := w.Run(context.Background(), func() {}); err == nil {
		t.Fatal("空路径必须返回错误")
	}
}

func TestDescribe(t *testing.T) {
	w := New("/etc/app/config.json")
	if got := w.Describe(); got == "" {
		t.Fatal("Describe 不能为空")
	}
	if w.Debounce != DefaultDebounce {
		t.Fatalf("默认去抖 = %v, want %v", w.Debounce, DefaultDebounce)
	}
}

func TestMatch(t *testing.T) {
	target := filepath.Clean("/etc/app/config.json")

	tests := []struct {
		name string
		ev   fsnotify.Event
		want bool
	}{
		{"写入目标", fsnotify.Event{Name: target, Op: fsnotify.Write}, true},
		{"替换目标", fsnotify.Event{Name: target, Op: fsnotify.Create}, true},
		{"删除目标", fsnotify.Event{Name: target, Op: fsnotify.Remove}, true},
		{"路径不规整", fsnotify.Event{Name: "/etc/app/../app/config.json", Op: fsnotify.Write}, true},
		{"其他文件", fsnotify.Event{Name: "/etc/app/other.json", Op: fsnotify.Write}, false},
		{"同目录临时文件", fsnotify.Event{Name: "/etc/app/.config.json.tmp", Op: fsnotify.Create}, false},
		{"纯 chmod 不改内容", fsnotify.Event{Name: target, Op: fsnotify.Chmod}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := match(tc.ev, target); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWatchedDirGone 把“目录失效”的判定钉死：只有目录（或其祖先）本身被删除、
// 改名才算失效，目录内单个文件的增删改一律不算。
func TestWatchedDirGone(t *testing.T) {
	const dir = "/etc/app"

	tests := []struct {
		name string
		ev   fsnotify.Event
		want bool
	}{
		{"目录被删除", fsnotify.Event{Name: dir, Op: fsnotify.Remove}, true},
		{"目录被改名", fsnotify.Event{Name: dir, Op: fsnotify.Rename}, true},
		{"祖先被删除", fsnotify.Event{Name: "/etc", Op: fsnotify.Remove}, true},
		{"目标文件被删除", fsnotify.Event{Name: dir + "/config.json", Op: fsnotify.Remove}, false},
		{"同目录其他文件被删除", fsnotify.Event{Name: dir + "/other.json", Op: fsnotify.Remove}, false},
		{"同前缀兄弟目录", fsnotify.Event{Name: "/etc/application", Op: fsnotify.Remove}, false},
		{"目录被写入", fsnotify.Event{Name: dir, Op: fsnotify.Write}, false},
		{"根目录", fsnotify.Event{Name: "/", Op: fsnotify.Remove}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := watchedDirGone(tc.ev, dir); got != tc.want {
				t.Fatalf("watchedDirGone = %v, want %v", got, tc.want)
			}
		})
	}
}
