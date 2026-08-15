package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/spec"
)

// Factory 由一条已解析的资源声明构造出资源实例。
type Factory func(env *Env, r spec.Resource) (Resource, error)

// factories 是已实现的资源类型。
var factories = map[string]Factory{
	TypeDirectory: newDirectory,
	TypeFile:      newFile,
	TypeTemplate:  newFile, // mechd 已把模板渲染为 content，两者在此同构
	TypeSymlink:   newSymlink,
	TypeArchive:   newArchive,
	TypeUser:      newUser,
	TypeGroup:     newGroup,
}

// plannedTypes 是 pack/v1 规范定义、但本版本尚未实现的类型。
//
// 单独列出是为了把「拼错了类型名」和「这个类型还没做」区分开——
// 前者用户改一个字母就好，后者要报「尚未实现」而不是「未知类型」。规范
// 是 draft-stable、资源类型集合不会随手改动，这份列表因此是静态的
// （docs/spec/pack-v1.md §14）。
//
// **不在这里写"计划哪个版本做"**：早先按里程碑标注过（如"M2 之后"），
// 项目经过 M2–M9 这些类型仍未实现，标注反而从「快了」变成了误导——
// 排期信息交给 design/25-roadmap.md 维护，不复制进代码里跟着一起过期。
// value 非空时是一条实现之外的额外原因（如 package 的 hermetic 顾虑），
// 空串就是单纯「还没做」。
var plannedTypes = map[string]string{
	"sysctl":       "",
	"limits":       "",
	"hosts_entry":  "",
	"mount":        "",
	"timer":        "",
	"systemd_unit": "",
	"command":      "",
	"script":       "",
	"package":      "非 hermetic，官方 Pack 不得使用",
}

// SupportedTypes 返回本版本已实现的资源类型，按字典序。
func SupportedTypes() []string {
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// New 由一条资源声明构造资源实例。
func New(env *Env, r spec.Resource) (Resource, error) {
	f, ok := factories[r.Type]
	if !ok {
		if detail, planned := plannedTypes[r.Type]; planned {
			if detail != "" {
				return nil, Permanentf("构造资源",
					"资源类型 %q 尚未实现（%s）", r.Type, detail)
			}
			return nil, Permanentf("构造资源", "资源类型 %q 尚未实现", r.Type)
		}
		return nil, Permanentf("构造资源",
			"未知的资源类型 %q（本版本支持：%s）",
			r.Type, strings.Join(SupportedTypes(), ", "))
	}
	res, err := f(env, r)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Build 按声明顺序构造整份资源清单。
//
// **构造阶段全部成功才返回**：参数写错是配置问题，应当在动手改机器之前
// 全部暴露出来，而不是执行到第三条时才发现第四条写错了。
func Build(env *Env, rs []spec.Resource) ([]Resource, error) {
	out := make([]Resource, 0, len(rs))
	var errs []string
	for i, r := range rs {
		res, err := New(env, r)
		if err != nil {
			errs = append(errs, fmt.Sprintf("  resources[%d] %s: %v", i, r.ID, err))
			continue
		}
		out = append(out, res)
	}
	if len(errs) > 0 {
		return nil, Permanentf("构造资源清单",
			"%d 条资源声明有问题：\n%s", len(errs), strings.Join(errs, "\n"))
	}
	return out, nil
}

// ── 参数解码 ────────────────────────────────────────────────────────────

// decodeArgs 把 Args 解到类型专属的结构。
//
// 未知字段报错而非忽略：`ower: webapp` 这样的拼写错误如果被静默丢掉，
// 用户会得到一个「看起来成功了但属主不对」的结果，而这类问题往往要到
// 服务起不来时才被发现。
func decodeArgs(r spec.Resource, v any) error {
	if len(r.Args) == 0 {
		return Permanentf("解析资源参数", "%s 资源 %s 没有参数", r.Type, r.ID)
	}
	dec := json.NewDecoder(bytes.NewReader(r.Args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Permanentf("解析资源参数", "%s 资源 %s: %v", r.Type, r.ID, err)
	}
	return nil
}

// badArg 构造一条参数错误。
func badArg(r spec.Resource, msg string) error {
	return Permanentf("解析资源参数", "%s 资源 %s: %s", r.Type, r.ID, msg)
}

// requireAbs 要求一个路径参数存在且为绝对路径。
//
// 相对路径在 agent 里没有意义——mechlet 的工作目录不是任何组件的目录，
// 一个相对路径会解析到完全不可预期的位置。
func requireAbs(r spec.Resource, field, v string) error {
	if v == "" {
		return badArg(r, "缺少 "+field)
	}
	if !filepath.IsAbs(v) {
		return badArg(r, fmt.Sprintf("%s 必须是绝对路径，实际 %q", field, v))
	}
	if strings.Contains(v, spec.GenerationPlaceholder) {
		return badArg(r, fmt.Sprintf(
			"%s 中残留了未替换的 %s——物化前必须先调用 spec.ResolveGeneration",
			field, spec.GenerationPlaceholder))
	}
	return nil
}
