package pack

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// 本文件对 YAML 节点树做外科式修改，而不是「解析成结构体再 Marshal 回去」。
//
// 理由：发布产物中的 pack.yaml 是**人会打开阅读**的东西。Marshal 会丢掉
// 作者写的全部注释、打乱字段顺序、把引号风格归一化——那是一份能用但读不懂
// 的产物。节点树修改能原样保留其余部分。

// mapIndex 返回 mapping 中某个键的下标（键节点的位置），不存在时返回 -1。
func mapIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// removeKey 从 mapping 中删除一个键值对，返回它原先所在的下标（-1 表示不存在）。
func removeKey(m *yaml.Node, key string) int {
	i := mapIndex(m, key)
	if i < 0 {
		return -1
	}
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	return i
}

// setKeyAt 设置键值对。已存在则原地替换（保留键节点上的注释）；
// 否则插入到 at 指定的下标，at < 0 表示追加到末尾。
func setKeyAt(m *yaml.Node, key string, val *yaml.Node, at int) {
	if i := mapIndex(m, key); i >= 0 {
		m.Content[i+1] = val
		return
	}
	kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	if at < 0 || at > len(m.Content) {
		m.Content = append(m.Content, kn, val)
		return
	}
	rest := append([]*yaml.Node{}, m.Content[at:]...)
	m.Content = append(append(m.Content[:at], kn, val), rest...)
}

// ── 构造节点 ────────────────────────────────────────────────────────────

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func quoted(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func intScalar(v int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)}
}

func seq(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle, Content: items}
}

func mapping(kv ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: kv}
}

// blobsNode 把计算好的 blob 清单构造成 YAML 节点。
func blobsNode(blobs map[string]Blob) *yaml.Node {
	root := mapping()
	for _, bn := range sortedKeys(blobs) {
		platforms := mapping()
		blob := blobs[bn]
		for _, plat := range sortedKeys(blob) {
			e := blob[plat]
			entry := mapping(
				scalar("sha256"), quoted(e.SHA256),
				scalar("size"), intScalar(e.Size),
				scalar("filename"), quoted(e.Filename),
			)
			if e.SourceURL != "" {
				entry.Content = append(entry.Content, scalar("sourceUrl"), quoted(e.SourceURL))
			}
			if e.MediaType != "" {
				entry.Content = append(entry.Content, scalar("mediaType"), scalar(e.MediaType))
			}
			platforms.Content = append(platforms.Content, scalar(plat), entry)
		}
		root.Content = append(root.Content, scalar(bn), platforms)
	}
	return root
}

// platformsNode 构造 platforms 列表节点。
func platformsNode(platforms []string) *yaml.Node {
	sorted := append([]string{}, platforms...)
	sort.Strings(sorted)
	items := make([]*yaml.Node, 0, len(sorted))
	for _, p := range sorted {
		items = append(items, scalar(p))
	}
	return seq(items...)
}
