package pack

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SourcesKey 是源 pack.yaml 中描述载荷来源的字段名。
// 它**不进入发布产物**——assemble 会用它算出 blobs 后将其移除。
const SourcesKey = "sources"

// Source 是一个平台的载荷来源。
//
// 支持两种写法：
//
//	main:
//	  linux/amd64: dist/app-amd64.tar.gz            # 简写
//	  linux/arm64: { path: dist/app-arm64.tar.gz, mediaType: tar.gz }
type Source struct {
	Path      string `yaml:"path"`
	MediaType string `yaml:"mediaType"`
	SourceURL string `yaml:"sourceUrl"`
	// Filename 覆盖记入 blob 的原始文件名；缺省取 Path 的 basename。
	Filename string `yaml:"filename"`
}

// UnmarshalYAML 支持字符串简写与对象两种形式。
func (s *Source) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s.Path = n.Value
		return nil
	case yaml.MappingNode:
		type plain Source // 避免递归调用本方法
		var p plain
		if err := n.Decode(&p); err != nil {
			return err
		}
		*s = Source(p)
		if s.Path == "" {
			return fmt.Errorf("line %d: source is missing path", n.Line)
		}
		return nil
	default:
		return fmt.Errorf("line %d: source must be a path string or a mapping", n.Line)
	}
}

// SourceSet 是 blob 名 → 平台 → 来源。
type SourceSet map[string]map[string]Source

// ParseSources 从原始 pack.yaml 文档中读出 sources 段。
// 没有该段时返回 nil。
func ParseSources(doc *yaml.Node) (SourceSet, error) {
	if doc == nil {
		return nil, nil
	}
	n := nodeAt(doc, SourcesKey)
	if n == nil {
		return nil, nil
	}
	var out SourceSet
	if err := n.Decode(&out); err != nil {
		return nil, fmt.Errorf("parsing %s block: %w", SourcesKey, err)
	}
	return out, nil
}

// HasSources 报告 pack.yaml 中是否声明了 sources 段。
func (p *Pack) HasSources() bool {
	return nodeAt(p.Doc, SourcesKey) != nil
}
