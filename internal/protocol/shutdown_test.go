package protocol

import (
	"context"
	"testing"
	"time"
)

// 本文件钉住**关闭路径不会挂住**。
//
// 缺陷长这样：`Subscribe` 是长连的服务端流，正常情况下永远不返回；而 grpc
// 的 `GracefulStop` 会等所有活跃 RPC 结束。两者撞在一起，mechd 收到 SIGTERM
// 之后一直挂到 systemd 的 `TimeoutStopSec` 再吃一发 SIGKILL。
//
// 它的代价不只是「重启慢半分钟」：SIGKILL 意味着 `defer st.Close()` 根本
// 走不到，SQLite 的 WAL 没有干净关闭。

// TestDrainReleasesGracefulStop 是这条修复的核心验收。
//
// **判据是 GracefulStop 真的返回了**，不是「Subscribe 收到了错误」——
// 后者可以在流仍然挂着的情况下成立，而挂住的正是关闭本身。
func TestDrainReleasesGracefulStop(t *testing.T) {
	h := newHarness(t)
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "demo", "server", "n1")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	go func() { _ = h.client.Subscribe(ctx, c) }()
	c.wait(t) // 等流真的建立起来——没有活跃流的话这条测试什么都没验

	h.srv.Drain()

	done := make(chan struct{})
	go func() { defer close(done); h.gs.GracefulStop() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Drain 之后 GracefulStop 仍然挂住——mechd 会被 systemd SIGKILL")
	}
}

// TestDrainIsIdempotent 钉住重复调用不 panic。
//
// 关闭路径最容易被调两次（信号处理与 defer 撞在一起），而在那里 panic
// 等于把一次正常退出变成一次崩溃退出。
func TestDrainIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.srv.Drain()
	h.srv.Drain()
	h.srv.Drain()
}

// TestDrainTellsTheNodeToReconnect 钉住节点收到的是「重连」而不是沉默。
//
// 对节点来说这与一次网络抖动没有区别，它本来就会重连——但**说清楚**要紧：
// 一个静默结束的流会让 agent 侧的日志只剩「连接断开」，而运维正在查的
// 可能恰恰是「谁把 mechd 停了」。
func TestDrainTellsTheNodeToReconnect(t *testing.T) {
	h := newHarness(t)
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "demo", "server", "n1")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	errc := make(chan error, 1)
	go func() { errc <- h.client.Subscribe(ctx, c) }()
	c.wait(t)

	h.srv.Drain()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("订阅应当以错误结束，好让 agent 知道要重连")
		}
		// 不断言具体措辞，只要说得出「正在关闭」这件事
		if got := err.Error(); got == "" {
			t.Error("错误信息不该是空的")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Drain 之后订阅流没有收摊")
	}
}
