package reconcile

import (
	"time"

	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/state"
)

// digestCache 把本机状态里的摘要缓存接到资源引擎上。
//
// 适配器放在这里而不是任一端，是为了不让 internal/state 与
// internal/resource 相互依赖：前者只管持久化，后者只管契约。
type digestCache struct {
	entries map[string]state.DigestEntry
	// touched 记录本轮实际用到的路径，用于淘汰已不再声明的条目。
	touched map[string]bool
	// Now 可替换，供测试固定时间。
	Now    func() time.Time
	hits   int
	misses int
}

func newDigestCache(in *state.Instance) *digestCache {
	m := make(map[string]state.DigestEntry, len(in.Digests))
	for k, v := range in.Digests {
		m[k] = v
	}
	return &digestCache{entries: m, touched: map[string]bool{}}
}

func (c *digestCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get 实现 resource.DigestCache。
func (c *digestCache) Get(path string, size int64, mod time.Time) (string, bool) {
	c.touched[path] = true
	e, ok := c.entries[path]
	if ok && e.Size == size && e.ModTime.Equal(mod) &&
		resource.DigestUsable(mod, e.CachedAt) {
		c.hits++
		return e.SHA256, true
	}
	c.misses++
	return "", false
}

// Put 实现 resource.DigestCache。
func (c *digestCache) Put(path string, size int64, mod time.Time, sum string) {
	c.touched[path] = true
	c.entries[path] = state.DigestEntry{
		Size: size, ModTime: mod, SHA256: sum, CachedAt: c.now(),
	}
}

// commit 把缓存写回实例状态，**只保留本轮碰过的路径**。
//
// 不淘汰的话，一个组件在生命周期里换过的每个文件路径都会永久留在状态文件里，
// 越滚越大，且没有任何用处。
func (c *digestCache) commit(in *state.Instance) {
	if len(c.touched) == 0 {
		return
	}
	out := make(map[string]state.DigestEntry, len(c.touched))
	for p := range c.touched {
		if e, ok := c.entries[p]; ok {
			out[p] = e
		}
	}
	in.Digests = out
}

var _ resource.DigestCache = (*digestCache)(nil)
