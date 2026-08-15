package spec

// ── 卸载时的路径归类 ────────────────────────────────────────────────────
//
// 这张表放在 spec 里而不是调和器里，因为**两端都要用同一份**：
// 节点侧照它决定删哪些目录，中心侧照它算出「这次 remove 会留下什么」
// 并在二档确认之前打给人看。两处各写一份的话，运维看到的预览迟早会
// 与真正发生的事情不一致——而那正是确认这个动作唯一的价值所在。

// Disposition 是一个 paths 条目在卸载时的处置。
type Disposition int

const (
	// DropAlways：无条件删。纯派生物，没有任何不可重建的内容。
	DropAlways Disposition = iota
	// DropUnlessKept：默认删，--keep-config 保留。
	DropUnlessKept
	// KeepUnlessPurged：默认保留并登记为孤儿，--purge-data 才删。
	KeepUnlessPurged
)

// DispositionOf 判断一个 paths 名在卸载时怎么处置（24-lifecycle §2.3）。
//
// **判据写死在引擎里，Pack 不能覆盖。** paths 的名字是 Pack 起的，
// 预定义名只有五个（pack-v1 §8.2），而 HDFS 那样的包会用 dataDirs
// 这类自定义名——归类不能只认预定义名，否则真正的数据盘会漏判。
//
// 因此**未归类的一律保留**：删错不可逆，留错可以 `orphans purge` 补救。
// dataDirs 正好落在这一行上。
//
// 调研过给 paths 加一个 onRemove 字段让 Pack 自己声明，没有采纳——
// pack-v1 现在是 draft-stable，不为目前两个示例包都用不上的字段扩张
// 格式。代价是 Pack 无法声明
// 「这个自定义目录是可丢的」：一个自定义 cache 目录每次卸载都会留一个
// 孤儿，只能手工 purge。真出现这个需求时再加字段，那是兼容的方向。
func DispositionOf(name string) Disposition {
	switch name {
	case "home", "runtime":
		// home 里是 generations 与 current 软链，全部由 Pack 载荷重建；
		// runtime 在 /run 下，本来就活不过重启。
		return DropAlways
	case "config":
		return DropUnlessKept
	default:
		// data / logs / 一切自定义名
		return KeepUnlessPurged
	}
}

// Drops 报告在给定开关下，这个处置是否意味着「删掉」。
//
// nil 开关按零值处理：配置删、数据留、用户留。
func (d Disposition) Drops(opts *Removal) bool {
	if opts == nil {
		opts = &Removal{}
	}
	switch d {
	case DropAlways:
		return true
	case DropUnlessKept:
		return !opts.KeepConfig
	default:
		return opts.PurgeData
	}
}
