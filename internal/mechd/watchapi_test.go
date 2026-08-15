package mechd

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// SSE 的判据只有一条真正要紧：**每条消息都是完整状态**（23-web-ui §4.5.1）。
//
// 推增量的实现也能让「第 N/M 批动起来」，因此不能拿那个当判据——
// 要判的是「漏掉一条之后还对不对」，而那等价于「每条消息自己就够用」。

// sseClient 起一个 httptest 服务器并连上 watch 流。
func sseClient(t *testing.T, f *fixture, comp string) (*bufio.Scanner, func()) {
	t.Helper()
	api := &API{S: f.svc, Auth: openAuth{}}
	srv := httptest.NewServer(api.Handler())

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET",
		srv.URL+APIPrefix+"/components/"+comp+"/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch 应当回 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应当是 text/event-stream，得到 %q", ct)
	}
	return bufio.NewScanner(resp.Body), func() {
		cancel()
		resp.Body.Close()
		srv.Close()
	}
}

// nextSnapshot 读到下一条 snapshot 事件；超时返回 nil。
func nextSnapshot(t *testing.T, sc *bufio.Scanner, within time.Duration) *WatchSnapshot {
	t.Helper()
	type got struct {
		snap *WatchSnapshot
	}
	ch := make(chan got, 1)
	go func() {
		var isSnap bool
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "event: snapshot":
				isSnap = true
			case strings.HasPrefix(line, "data: ") && isSnap:
				var s WatchSnapshot
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &s); err == nil {
					ch <- got{&s}
					return
				}
				isSnap = false
			}
		}
		ch <- got{nil}
	}()
	select {
	case g := <-ch:
		return g.snap
	case <-time.After(within):
		return nil
	}
}

// TestWatchPushesAFullSnapshotImmediately 是 §4.5.1 的核心。
//
// 连上就该收到一份完整状态，而不是「等下一次变化」。少了这条，页面在
// 一个平静的集群上会一直空着——而那看起来像接口坏了。
func TestWatchPushesAFullSnapshotImmediately(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	sc, done := sseClient(t, f, "paramkit")
	defer done()

	snap := nextSnapshot(t, sc, 5*time.Second)
	if snap == nil {
		t.Fatal("连上之后没有立刻收到快照")
	}
	if snap.Component != "paramkit" {
		t.Errorf("快照的组件名不对: %q", snap.Component)
	}
	// **完整**：状态里该有实例，不是一个「有变化」的空信号
	if snap.Status == nil || len(snap.Status.Instances) == 0 {
		t.Fatal("快照里应当带着完整的 status——推增量的实现会在这里露馅")
	}
	if snap.At == "" {
		t.Error("快照应当带时间戳")
	}
}

// TestWatchDoesNotRepeatAnUnchangedSnapshot 守的是「没变就不推」。
//
// 兜底的定时唤醒每 5 秒算一次，若不比对就推，客户端会在一个什么都没
// 发生的集群上每 5 秒白重绘一次。
func TestWatchDoesNotRepeatAnUnchangedSnapshot(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	sc, done := sseClient(t, f, "paramkit")
	defer done()

	if first := nextSnapshot(t, sc, 5*time.Second); first == nil {
		t.Fatal("没收到第一条快照")
	}
	// 什么都不改，等过一个兜底周期（5s）再多一点
	if again := nextSnapshot(t, sc, 8*time.Second); again != nil {
		t.Errorf("状态没变却又推了一条快照:\n  %+v\n"+
			"  兜底重算是必要的，但推之前要比对", again.Status)
	}
}

// TestWatchPushesAgainWhenStateChanges 是验收表第 14 条的服务端一半。
//
// 「第 N/M 批自动更新」在服务端就是这件事：状态一变就推，且推的是**完整
// 的新状态**——客户端不需要知道变了什么，只需要用新的覆盖旧的。
func TestWatchPushesAgainWhenStateChanges(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	sc, done := sseClient(t, f, "paramkit")
	defer done()

	first := nextSnapshot(t, sc, 5*time.Second)
	if first == nil {
		t.Fatal("没收到第一条快照")
	}

	// 改一个参数：期望 digest 变，状态随之变
	if _, err := f.svc.SetParams(ctx(), SetParamsRequest{
		Site: DefaultSite, Component: "paramkit",
		Set: map[string]any{"p_int": 44}, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	next := nextSnapshot(t, sc, 10*time.Second)
	if next == nil {
		t.Fatal("状态变了却没有推新快照")
	}
	if next.Status == nil || len(next.Status.Instances) == 0 {
		t.Fatal("第二条也必须是完整状态，不能是增量")
	}

	var moved bool
	for i, inst := range next.Status.Instances {
		if i < len(first.Status.Instances) && inst.Want != first.Status.Instances[i].Want {
			moved = true
		}
	}
	if !moved {
		t.Error("新快照与旧的一模一样——推送的时机对了，内容没跟上")
	}
}

// TestWatchRejectsUnknownComponent 守的是「打错名字」有个明确的答案。
//
// 回一条永远不推任何东西的流，在界面上表现为「一直在加载」——
// 那是最难查的一类现场。
func TestWatchRejectsUnknownComponent(t *testing.T) {
	f := formFixture(t)
	api := &API{S: f.svc, Auth: openAuth{}}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + APIPrefix + "/components/nosuch/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("不存在的组件应当直接回错误，而不是一条空流")
	}
}

// openAuth 是测试用的放行认证。
//
// 认证本身在 authapi_test.go 里验透了；这里要验的是 SSE 的行为，
// 而不是「它有没有被守住」。
type openAuth struct{}

func (openAuth) Authenticate(*http.Request) (string, error) { return "admin", nil }

// TestWatchRecomputesAtMostOncePerSecond 守的是重算的**上限**。
//
// 这条是拿真集群跑出来的：Report 里每个实例都会 bump 一次，而
// `reportedAt` 每次上报都变——状态确实变了，快照也确实该推。问题在代价：
// 一个 50 实例的组件、15 秒上报一次，就是每秒 3 次完整的解析管线重算，
// 而每个开着的浏览器各一份。
//
// Hub 的容量-1 通道只在消费者**慢**时合并；这条测试守的是消费者快时
// 也有上限——判据是「一秒内敲一百次 bump，推送次数远小于一百」。
func TestWatchRecomputesAtMostOncePerSecond(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	f.svc.Watch = NewHub()

	sc, done := sseClient(t, f, "paramkit")
	defer done()

	if nextSnapshot(t, sc, 5*time.Second) == nil {
		t.Fatal("没收到第一条快照")
	}

	// 一秒内狂敲，每次都真的改一点状态（否则会被摘要比对挡掉，
	// 那样测的就不是节流而是去重了）
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = f.svc.SetParams(ctx(), SetParamsRequest{
				Site: DefaultSite, Component: "paramkit",
				Set: map[string]any{"p_int": 10 + i%40}, Actor: "t",
			})
			f.svc.Watch.Bump("paramkit")
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer close(stop)

	// 数 3 秒内推了几条
	deadline := time.Now().Add(3 * time.Second)
	n := 0
	for time.Now().Before(deadline) {
		if s := nextSnapshot(t, sc, time.Until(deadline)); s == nil {
			break
		}
		n++
	}
	// 3 秒 × 每秒至多 1 次，留一倍余量
	if n > 6 {
		t.Errorf("3 秒内推了 %d 条——重算没有上限，"+
			"一个大集群会让每个开着的页面把 mechd 拖垮", n)
	}
	if n == 0 {
		t.Error("一条都没推——节流过头了，界面不会更新")
	}
}
