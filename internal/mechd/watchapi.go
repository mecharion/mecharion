package mechd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SSE 的两个节拍。
const (
	// watchIdle 是没有收到任何提示时的兜底重算周期。
	//
	// bump 丢了就靠它补上——**事件只是提示，状态才是真相**。
	// 少了它，一次漏掉的 bump 会让页面停在错误的状态上直到用户手动刷新。
	watchIdle = 5 * time.Second
	// watchMin 是两次重算之间的**下限**。
	//
	// 这条是拿真集群跑出来的：Report 里每个实例都会 bump 一次，而
	// `reportedAt` 每次上报都变——于是状态"确实变了"，快照也确实该推。
	// 问题在代价：一个 50 实例的组件、15 秒上报一次，就是每秒 3 次
	// **完整的解析管线重算**，而每个开着的浏览器各一份。
	//
	// Hub 的容量-1 通道只在消费者**慢**的时候合并；这里补的是消费者
	// 快的时候。代价是界面最多晚 1 秒——对一个人眼在看的页面，
	// 那不是可感知的差别。
	watchMin = time.Second
	// watchPing 是心跳周期。
	//
	// 反向代理与负载均衡普遍在 30–60 秒空闲后关连接，而一个没有 rollout
	// 在跑的组件可能几十分钟没有推送。注释行（": ping"）不会被
	// EventSource 当成消息，只是让连接上有字节流动。
	watchPing = 15 * time.Second
)

// WatchSnapshot 是一次推送的内容。
//
// **它是完整状态，不是增量**（23-web-ui §4.5.1）。客户端的处理是
// 「用收到的这份覆盖掉手上那份」——不需要按类型套用 patch，也不需要
// 补发机制：断线重连之后的第一条就把状态修正了。
type WatchSnapshot struct {
	Component string       `json:"component"`
	Status    *StatusView  `json:"status,omitempty"`
	Rollout   *RolloutView `json:"rollout,omitempty"`
	At        string       `json:"at"`
}

// watch 是组件详情页的 SSE 流。
//
// 认证只能靠 **session cookie**：`EventSource` 不能设请求头，这是浏览器
// API 的硬限制。现有的 guard 对 GET 不要求 CSRF 头，因此这里不需要开口子
// ——但**若将来有人把 CSRF 检查从「写操作」放宽成「所有请求」，SSE 会
// 当场断掉而原因难查**（23-web-ui §4.5.4）。
func (a *API) watch(w http.ResponseWriter, r *http.Request, _ string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.writeErr(w, r, http.StatusInternalServerError,
			fmt.Errorf("this HTTP server does not support streaming responses"))
		return
	}
	name := r.PathValue("name")
	site := r.URL.Query().Get("site")

	// 先确认组件存在：让「组件名打错了」得到一个 404，而不是一条
	// 永远不推任何东西的流——后者在界面上表现为「一直在加载」。
	if _, _, err := a.S.componentOf(r.Context(), site, name); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	// 反向代理的缓冲会把 SSE 变成「什么都不来，然后一次全来」。
	// nginx 认这个头；它对别的代理无害。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 断线重连间隔。EventSource 默认 3 秒，这里显式写出来是为了让
	// 「重连多久一次」在代码里看得见，而不是散落在浏览器默认值里。
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	var sub <-chan struct{}
	var unsub func()
	if a.S.Watch != nil {
		sub, unsub = a.S.Watch.Subscribe(name)
		defer unsub()
	}

	// **重算由一个固定节拍驱动，bump 只是把 dirty 立起来。**
	//
	// 直接在 bump 上重算会让重算频率跟着集群规模走（见 watchMin）；
	// 这样它有一个与规模无关的上限。
	tick := time.NewTicker(watchMin)
	defer tick.Stop()
	ping := time.NewTicker(watchPing)
	defer ping.Stop()

	dirty := false
	// **用真实时钟，不用 s.now()。** 这是 HTTP 流的节拍，不是领域时间；
	// 可替换的那个是给 rollout 门禁用的，混在一起会让一个冻住时钟的
	// 测试把这里变成每秒重算。
	lastCompute := time.Now()

	var last string
	send := func() bool {
		snap, sum := a.snapshot(r.Context(), site, name)
		if snap == nil || sum == last {
			return true // 没变就不推：一条没有信息的消息只会让客户端白重绘
		}
		last = sum
		b, err := json.Marshal(snap)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// 连上就先推一份：客户端不必等到下一次变化才有内容可显示
	if !send() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub:
			dirty = true
		case now := <-tick.C:
			// 兜底：即使没收到任何提示，也每 watchIdle 重算一次。
			// bump 丢了就靠它补上——事件只是提示，状态才是真相。
			stale := now.Sub(lastCompute) >= watchIdle
			if !dirty && !stale {
				continue
			}
			dirty = false
			lastCompute = now
			if !send() {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// snapshot 算一份当前状态，并返回它的摘要用于「变了没有」的判断。
//
// 用**序列化之后的摘要**而不是逐字段比：字段会加，而一个忘了更新的
// 比较函数会让新加的字段永远推不出去——那种缺陷在界面上表现为
// 「某一列不会自己更新」，极难联想到这里。
func (a *API) snapshot(ctx context.Context, site, name string) (*WatchSnapshot, string) {
	snap := &WatchSnapshot{Component: name}

	if st, err := a.S.Status(ctx, site, name); err == nil {
		snap.Status = st
	}
	// 没升级过的组件查不到 rollout，那不是错误，只是没有可看的
	if ro, err := a.S.RolloutStatus(ctx, site, name); err == nil {
		snap.Rollout = ro
	}
	if snap.Status == nil && snap.Rollout == nil {
		return nil, ""
	}

	// At 不参与摘要：它每次都不同，算进去等于每次都「变了」
	b, err := json.Marshal(struct {
		S *StatusView  `json:"s"`
		R *RolloutView `json:"r"`
	}{snap.Status, snap.Rollout})
	if err != nil {
		return nil, ""
	}
	sum := sha256.Sum256(b)
	snap.At = a.S.now().UTC().Format(time.RFC3339)
	return snap, hex.EncodeToString(sum[:])
}
