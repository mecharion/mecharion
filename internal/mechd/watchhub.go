package mechd

import "sync"

// Hub 把「有东西变了」这件事通知给正在看的浏览器（23-web-ui §4.5.2）。
//
// **它只传「催一下」这个信号，不传内容。** 订阅者收到之后自己去算一份
// 完整的当前状态。这是刻意的：
//
//	带内容 → bump 变成不能丢的东西 → 要缓冲、要重试、要背压
//	不带   → bump 丢了顶多晚一点，兜底的定时唤醒会补上
//
// 与整个项目的那条纪律是同一个形状——**事件只是提示，状态才是真相**。
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe 订阅某个组件的变化，返回通道与退订函数。
func (h *Hub) Subscribe(component string) (<-chan struct{}, func()) {
	// **容量 1 的通道**：订阅者慢的时候，多次 bump 合并成一次唤醒。
	// 那正是想要的语义——它收到之后会去读**当时**的状态，
	// 中间漏掉几次提示不改变结果。
	ch := make(chan struct{}, 1)

	h.mu.Lock()
	if h.subs[component] == nil {
		h.subs[component] = map[chan struct{}]struct{}{}
	}
	h.subs[component][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m := h.subs[component]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				// 空了就把这一项删掉，否则一个长期运行的 mechd 会
				// 攒下每一个被看过的组件名
				delete(h.subs, component)
			}
		}
	}
}

// Bump 提示某个组件有变化。
//
// **绝不阻塞。** 它被调用的地方是 mechlet 上报与 rollout 推进这两条
// 热路径上——一个慢的浏览器不该拖住集群的状态收敛。
func (h *Hub) Bump(component string) {
	h.mu.Lock()
	subs := make([]chan struct{}, 0, len(h.subs[component]))
	for ch := range h.subs[component] {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // 已经有一个没被取走的提示了，够了
		}
	}
}

// bump 是 Service 侧的便捷入口，Hub 未接线时什么也不做。
//
// 允许为 nil：服务层的正确性与「谁在看」无关，单元测试不该被迫接一个 Hub。
func (s *Service) bump(component string) {
	if s.Watch != nil {
		s.Watch.Bump(component)
	}
}
