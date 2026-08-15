// Package ident 校验运维手填的标识符：Component、Node、Site、ConfigGroup。
//
// 这些名字会被直接拼进 agent 本地文件名、默认受管路径与离线证书文件名，
// 此前完全没有统一校验。规则复用 Pack 自己
// 早已强制的 DNS label（`mechpack lint` 的 R02/R09），让「名字」在整个
// 项目里只有一种合法形态。见 docs/design/09-naming-conventions.md §7。
package ident

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Kind 标记标识符属于哪一类实体，只用于生成错误信息里的名词。
type Kind int

const (
	Component Kind = iota
	Node
	Site
	ConfigGroup
)

func (k Kind) label() string {
	switch k {
	case Component:
		return "组件名"
	case Node:
		return "节点名"
	case Site:
		return "站点名"
	case ConfigGroup:
		return "配置组名"
	default:
		return "标识符"
	}
}

// ErrInvalid 是全部非法标识符的哨兵，稳定可判（errors.Is），
// 同时也是错误文案里稳定可判的 invalid_identifier 固定前缀。
var ErrInvalid = errors.New("invalid_identifier")

// label 是 RFC 1123 label：小写字母、数字、连字符，首尾不能是连字符。
// 与 internal/pack/lint_rules.go 的 dnsLabel（规范 R02/R09）是同一条规则——
// 两处独立维护是因为 pack 校验的是 Pack 文件里的 name/Role.name，
// ident 校验的是运维手填的运行期实体名，语义上不是同一件事，但字符集
// 故意保持一致，好让「合法名字」在全项目里只有一种形态。
var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

const (
	maxLabelLen     = 63
	maxSubdomainLen = 253
	// echoLimit 是错误信息里回显原始输入的上限，防止超长输入把日志/
	// 错误响应撑爆——这本身也是被拒绝的输入之一，不需要完整回显。
	echoLimit = 80
)

// Validate 校验一个标识符。除 Node 外都是单段 RFC 1123 label；
// Node 是点号分隔的 RFC 1123 subdomain——默认取自 os.Hostname()，
// 真实主机名常见 FQDN 形式（含点号），单段 label 规则会拒绝掉大量
// 合法主机名。
func Validate(kind Kind, name string) error {
	if kind == Node {
		return validateSubdomain(kind, name)
	}
	return validateLabel(kind, name)
}

func validateLabel(kind Kind, name string) error {
	if name == "" {
		return fail(kind, name, "不能为空")
	}
	if len(name) > maxLabelLen {
		return fail(kind, name, fmt.Sprintf("长度 %d 超过上限 %d", len(name), maxLabelLen))
	}
	if !labelRe.MatchString(name) {
		return fail(kind, name, "只能包含小写字母、数字与连字符，且不能以连字符开头或结尾")
	}
	return nil
}

func validateSubdomain(kind Kind, name string) error {
	if name == "" {
		return fail(kind, name, "不能为空")
	}
	if len(name) > maxSubdomainLen {
		return fail(kind, name, fmt.Sprintf("长度 %d 超过上限 %d", len(name), maxSubdomainLen))
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > maxLabelLen || !labelRe.MatchString(label) {
			return fail(kind, name,
				"必须是点号分隔的合法标签（RFC 1123），每段只能包含小写字母、数字与连字符,"+
					"首尾不能是连字符或点号,每段不超过 63 字符")
		}
	}
	return nil
}

func fail(kind Kind, name, reason string) error {
	display := name
	if len(display) > echoLimit {
		display = fmt.Sprintf("%s…（共 %d 字符）", display[:echoLimit], len(display))
	}
	return fmt.Errorf("%w: %s %q %s", ErrInvalid, kind.label(), display, reason)
}
