package poll

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestPollReportsChanges 覆盖：首次基线、内容不变、内容变化、删除、重建。
func TestPollReportsChanges(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	w := New(path)

	if !w.Poll() {
		t.Fatal("首次 Poll 必须报告变化（还没有基线）")
	}
	if w.State() == "unobserved" {
		t.Fatal("首次 Poll 之后不应再是 unobserved")
	}
	if !strings.Contains(w.Describe(), path) {
		t.Fatalf("Describe 未包含路径: %s", w.Describe())
	}

	if w.Poll() {
		t.Fatal("内容未变时不应报告变化")
	}

	writeFile(t, path, `{"addr":"b"}`)
	if !w.Poll() {
		t.Fatal("内容变化必须报告")
	}
	if w.Poll() {
		t.Fatal("同一内容重复读取不应报告变化")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !w.Poll() {
		t.Fatal("文件消失必须报告")
	}
	if w.State() != "missing" {
		t.Fatalf("State = %q, want missing", w.State())
	}
	if w.Poll() {
		t.Fatal("持续缺失不应重复报告")
	}

	writeFile(t, path, `{"addr":"c"}`)
	if !w.Poll() {
		t.Fatal("文件重建必须报告")
	}
}

// TestPollDetectsAtomicReplace 覆盖编辑器的"写临时文件 + rename"模式。
func TestPollDetectsAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	writeFile(t, path, `{"addr":"old"}`)

	w := New(path)
	if !w.Poll() {
		t.Fatal("首次 Poll 必须报告变化")
	}
	if w.Poll() {
		t.Fatal("内容未变时不应报告变化")
	}

	tmp := filepath.Join(dir, ".app.json.tmp")
	writeFile(t, tmp, `{"addr":"new"}`)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !w.Poll() {
		t.Fatal("原子替换必须报告变化")
	}
	if w.Poll() {
		t.Fatal("替换后再读同一内容不应报告变化")
	}
}

// TestPollReportsUnreadableFile 覆盖非"不存在"的读取错误：也必须报告一次，
// 这样 Manager 才能记录错误。
func TestPollReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时 chmod 000 仍然可读")
	}
	path := newFile(t, `{"addr":"a"}`)
	w := New(path)
	if !w.Poll() {
		t.Fatal("首次 Poll 必须报告变化")
	}

	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if !w.Poll() {
		t.Fatal("读取失败必须报告变化")
	}
	if !strings.HasPrefix(w.State(), "error: ") {
		t.Fatalf("State = %q, want error prefix", w.State())
	}
	if w.Poll() {
		t.Fatal("同一个读取错误不应重复报告")
	}

	// 恢复权限：状态再次变化。
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if !w.Poll() {
		t.Fatal("恢复可读必须报告变化")
	}
}

// waitBaselined 等待 Run 完成启动时的基线观察。基线之前调用 notify 是无意义的，
// 因此测试必须先确认它已完成，才去改动文件。
func waitBaselined(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for w.State() == "unobserved" {
		if time.Now().After(deadline) {
			t.Fatal("监听器未建立基线")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunNotifiesOnChange 用注入的 tick 通道驱动，不依赖真实时间间隔。
func TestRunNotifiesOnChange(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	w := New(path)
	ticks := make(chan time.Time, 4)
	w.ticks = ticks

	notified := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, func() { notified <- struct{}{} }) }()

	// 启动对齐：注册完成后立即通知一次。
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("未收到启动对齐通知")
	}
	waitBaselined(t, w)
	if w.State() == "unobserved" {
		t.Fatal("启动对齐后应已建立基线")
	}

	// 未变化：tick 不应产生通知。
	ticks <- time.Now()
	select {
	case <-notified:
		t.Fatal("内容未变时不应通知")
	case <-time.After(50 * time.Millisecond):
	}

	writeFile(t, path, `{"addr":"b"}`)
	ticks <- time.Now()
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("内容变化后未收到通知")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run 在 ctx 取消后必须返回 nil, got %v", err)
	}
}

// TestRunStopsOnCancel 覆盖停止语义：Run 返回后不再产生通知。
func TestRunStopsOnCancel(t *testing.T) {
	path := newFile(t, `{"addr":"a"}`)
	w := New(path)
	ticks := make(chan time.Time, 4)
	w.ticks = ticks

	notified := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, func() { notified <- struct{}{} }) }()

	// 收掉启动对齐的通知，确保后面的断言针对“停止后”的新通知。
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("未收到启动对齐通知")
	}
	waitBaselined(t, w)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	// Run 已退出：再注入 tick 与改动内容都不会产生通知。
	writeFile(t, path, `{"addr":"b"}`)
	ticks <- time.Now()
	select {
	case <-notified:
		t.Fatal("Run 返回后不应再有通知")
	default:
	}

	// 重复取消是安全的。
	cancel()
}

func TestRunRejectsEmptyPath(t *testing.T) {
	w := &Watcher{}
	if err := w.Run(context.Background(), func() {}); err == nil {
		t.Fatal("空路径必须返回错误")
	}
}
