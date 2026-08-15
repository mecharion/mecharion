package mechd

import (
	"sync"
	"testing"
	"time"
)

// Hub 的判据集中在一件事上：**它只传提示，丢了不影响正确性**。
//
// 因此这里测的不是「每次 Bump 都收得到」——那恰恰不是它承诺的东西。
// 测的是「不阻塞」「会合并」「退订之后不再收到」。

func TestBumpNeverBlocksOnASlowWatcher(t *testing.T) {
	h := NewHub()
	_, unsub := h.Subscribe("web") // 订了但**从不读**
	defer unsub()

	// Bump 被调用的地方是 mechlet 上报与 rollout 推进这两条热路径上。
	// 一个慢的浏览器不该拖住集群的状态收敛——因此这里连着敲很多次，
	// 任何一次阻塞都会让这条测试挂在超时上。
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Bump("web")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Bump 阻塞了——一个不读的订阅者把上报这条路径拖住了")
	}
}

func TestBumpsCollapse(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("web")
	defer unsub()

	for i := 0; i < 5; i++ {
		h.Bump("web")
	}

	// 收到一次
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("五次 Bump 一次都没收到")
	}
	// **不该有第二次**：订阅者收到提示后会去读当时的状态，
	// 中间漏掉几次提示不改变结果。攒着它们只会让客户端白重算几遍。
	select {
	case <-ch:
		t.Error("多次 Bump 应当合并成一次唤醒")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBumpOnlyReachesItsOwnComponent(t *testing.T) {
	h := NewHub()
	web, unsubWeb := h.Subscribe("web")
	defer unsubWeb()
	db, unsubDB := h.Subscribe("db")
	defer unsubDB()

	h.Bump("web")

	select {
	case <-web:
	case <-time.After(time.Second):
		t.Fatal("web 的订阅者没收到 web 的提示")
	}
	select {
	case <-db:
		t.Error("db 的订阅者收到了 web 的提示——一个开着 A 页面的浏览器" +
			"会因为 B 组件的上报而不停重算")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestUnsubscribeStopsAndCleansUp 守的是长期运行的 mechd 不会攒垃圾。
//
// 一个只订过一次的组件名若留在 map 里，一个跑几个月的 mechd 会攒下
// 每一个被人看过的组件——那是慢性泄漏，不会有人注意到。
func TestUnsubscribeStopsAndCleansUp(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("web")
	unsub()

	h.Bump("web")
	select {
	case <-ch:
		t.Error("退订之后不该再收到提示")
	case <-time.After(100 * time.Millisecond):
	}

	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("最后一个订阅者走了之后应当把这一项删掉，还剩 %d 项", n)
	}
}

func TestConcurrentSubscribeAndBump(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ch, unsub := h.Subscribe("web")
			<-time.After(time.Millisecond)
			unsub()
			_ = ch
		}()
		go func() {
			defer wg.Done()
			h.Bump("web")
		}()
	}
	wg.Wait() // -race 下跑：并发订阅与提示不该互相踩
}

// TestNilHubIsHarmless 守的是服务层不被迫接一个 Hub。
//
// 服务层的正确性与「谁在看」无关。让 Service 必须有 Hub 才能工作，
// 会让每个单元测试都要接一个它不关心的东西。
func TestNilHubIsHarmless(t *testing.T) {
	s := &Service{}
	s.bump("web") // 不该 panic
}
