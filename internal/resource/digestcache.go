package resource

import (
	"os"
	"sync"
	"time"
)

// DigestCache 用 (size, mtime) 免掉重复的整文件哈希。
//
// 调和每 60 秒跑一次，而 `file` / `template` 资源的 Read 需要文件的 sha256
// 才能比对内容。一个装了十个组件、每个带 50MB 二进制的节点，不加缓存就是
// **每分钟读取并哈希 500MB**——而绝大多数轮次里什么都没变。
//
// 实现放在调用方（调和器把它接到本机状态上），这里只定义契约，避免
// internal/state 与 internal/resource 相互依赖。
type DigestCache interface {
	// Get 返回 path 在给定 (size, mtime) 下缓存的摘要。
	Get(path string, size int64, mod time.Time) (string, bool)
	// Put 记录一条摘要。实现必须同时记下**记录时刻**，见下方 racy 说明。
	Put(path string, size int64, mod time.Time, sum string)
}

// 关于 racily clean
//
// 只比 (size, mtime) 有一个致命窗口：**文件在「我们记录摘要的同一刻度」
// 被改写，且大小没变**。此后 stat 永远与缓存相符，那条内容再也不会被
// 重新读取——漂移检测对它永久失明。
//
// 这不是理论问题：`port: 8080` 改成 `port: 1234` 恰好同样长度，而配置
// 渲染与人工编辑完全可能落在同一秒（某些文件系统的 mtime 粒度就是 1 秒）。
//
// git 的索引遇到过同一问题，解法是：**mtime 不严格早于记录时刻的条目
// 一律当作脏的**，重新哈希一次。代价是每次写入后多算一遍，之后才进入
// 稳态；换来的是这个窗口被彻底关掉。

// hashFileCached 优先走缓存，(size, mtime) 任一不符才真正哈希。
//
// **这是一个刻意的取舍**：内容改了而大小与 mtime 都原样恢复的情况检测不到。
// rsync、Ansible 的默认行为同样如此——代价是构造这种文件需要专门下功夫，
// 收益是常态下把「每轮全量哈希」降为「每轮一次 stat」。
//
// 需要确定性结论时走 `mechctl component verify`（强制全量哈希），
// 那是低频的人工动作，不该让调和循环为它买单。
func hashFileCached(env *Env, path string) (string, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	size, mod := fi.Size(), fi.ModTime()

	if c := env.digests(); c != nil {
		if sum, ok := c.Get(path, size, mod); ok {
			return sum, size, nil
		}
	}

	sum, n, err := hashFile(path)
	if err != nil {
		return "", 0, err
	}
	if c := env.digests(); c != nil {
		// 用哈希**之后**重新读到的 stat 存缓存：若文件正在被写，
		// 此刻的 (size, mtime) 与内容才是配套的；用哈希前的值会把一份
		// 半截内容的摘要钉在新的 mtime 上，之后再也不会被重算。
		if fi2, err2 := os.Stat(path); err2 == nil {
			c.Put(path, fi2.Size(), fi2.ModTime(), sum)
		}
	}
	return sum, n, nil
}

// recordDigest 在**刚写完一个文件**时登记它的摘要。
//
// 写入方本来就知道内容是什么，没必要等下一轮再读一遍磁盘算出来。
// 失败一律忽略：缓存是优化，不是正确性的一部分。
func recordDigest(env *Env, path, sum string) {
	c := env.digests()
	if c == nil || sum == "" {
		return
	}
	if fi, err := os.Stat(path); err == nil {
		c.Put(path, fi.Size(), fi.ModTime(), sum)
	}
}

// MemoryDigestCache 是一个进程内的实现，供测试与无状态场景使用。
type MemoryDigestCache struct {
	mu sync.Mutex
	m  map[string]memEntry
	// Hits / Misses 供诊断，说明缓存到底有没有起作用。
	Hits, Misses int
}

type memEntry struct {
	size     int64
	mod      time.Time
	sum      string
	cachedAt time.Time
}

// NewMemoryDigestCache 构造一个空缓存。
func NewMemoryDigestCache() *MemoryDigestCache {
	return &MemoryDigestCache{m: map[string]memEntry{}}
}

// Get 实现 DigestCache。
func (c *MemoryDigestCache) Get(path string, size int64, mod time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[path]
	if ok && e.size == size && e.mod.Equal(mod) && DigestUsable(mod, e.cachedAt) {
		c.Hits++
		return e.sum, true
	}
	c.Misses++
	return "", false
}

// Put 实现 DigestCache。
func (c *MemoryDigestCache) Put(path string, size int64, mod time.Time, sum string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]memEntry{}
	}
	c.m[path] = memEntry{size: size, mod: mod, sum: sum, cachedAt: time.Now()}
}

// RacyWindow 是 mtime 必须比记录时刻早出的幅度。
//
// 取 1 秒是为了覆盖**最粗的 mtime 粒度**（ext3、FAT、部分 NFS 就是 1 秒）。
// 只要 mtime 比记录时刻早 1 秒以上，任何后续写入都必然产生不同的 mtime，
// 缓存才可信。
//
// 生产上这条几乎不花钱：文件是在上一次调和时写的，而下一轮在 60 秒之后。
// 只有「刚写完立刻再调和」才会多算一次哈希——那恰恰是最该重算的时刻。
const RacyWindow = time.Second

// DigestUsable 报告一条缓存是否可信。
//
// 判据不是「mtime 早于记录时刻」——`Put` 本来就发生在写入之后，那样恒成立，
// 检查形同虚设。真正的危险窗口是**文件系统 mtime 粒度内的后续写入**：
// 若粒度是 1 秒，在同一秒内把 `port: 8080` 改成 `port: 1234`（长度恰好相同），
// stat 结果与缓存完全一致，这处漂移会**永久**发现不了。
//
// 因此要求 mtime 比记录时刻早出至少 RacyWindow。
//
// 导出是为了让持久化端与内存端用**同一条判据**，不至于一边严一边松。
func DigestUsable(mod, cachedAt time.Time) bool {
	if cachedAt.IsZero() {
		return false
	}
	return cachedAt.Sub(mod) > RacyWindow
}
