package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/pack"
)

// DefaultExportFormat 是单实例的默认格式（spec §5.3）。
const DefaultExportFormat = "{{ .Address }}:{{ .Port }}"

// DefaultExportSeparator 是多实例的默认连接符。
const DefaultExportSeparator = ","

// evalExports 求值 Pack 声明的对外连接点。
//
// 这是 **Pack 之间唯一被推荐的耦合方式**：消费方只引用导出名，不伸手进
// 提供方的角色内部。提供方把角色改名或换端口时，只要导出名不变，
// 消费方就不受影响（spec §5.3）。
//
// 两种形态：
//
//	format  多实例拼成一串（ZK 的 client 连接串）——形状是确定的
//	fields  具名字段，由消费方自己组装（带凭据的连接）——提供方
//	        没法知道消费方要 libpq DSN 还是 JDBC URL，让它猜等于
//	        把消费方的实现细节写进提供方（spec §5.4）
func (r *run) evalExports(insts []*instance, topo map[string]*TopologyCtx) (map[string]Export, error) {
	if len(r.req.Pack.Exports) == 0 {
		return nil, nil
	}
	out := map[string]Export{}

	for _, name := range sortedExportNames(r.req.Pack.Exports) {
		e := r.req.Pack.Exports[name]
		members := instancesOfRole(insts, e.Role)
		if len(members) == 0 {
			// 该角色没部署：导出不存在，而不是导出一个空串。
			// 空串会让消费方拿到一个「看起来有值」的连接串，
			// 然后在运行期以难以理解的方式失败。
			r.warnf("%s: export %s declares role %s as its provider, but that role has no instances, skipping this export",
				r.req.Component, name, e.Role)
			continue
		}

		ex, err := r.evalOneExport(name, e, members, topo)
		if err != nil {
			return nil, err
		}
		out[name] = ex
	}
	return out, nil
}

func (r *run) evalOneExport(
	name string, e pack.Export, members []*instance, topo map[string]*TopologyCtx,
) (Export, error) {
	// 端口在**第一个实例**的上下文里求值：同一角色的端口参数取值相同，
	// 否则连接串本身就没有意义
	head := members[0]
	headCtx := r.exportCtx(head, topo)

	port, err := r.eng.Expr("exports."+name+".port", e.Port, headCtx)
	if err != nil {
		return Export{}, err
	}

	if len(e.Fields) > 0 {
		return r.evalFields(name, e, head, headCtx, port)
	}

	// format 形态：逐实例格式化再连起来
	format := orDefault(e.Format, DefaultExportFormat)
	sep := orDefault(e.Separator, DefaultExportSeparator)

	parts := make([]string, 0, len(members))
	for _, m := range members {
		ctx := r.exportCtx(m, topo)
		ctx.Port = port
		s, err := r.eng.Expr("exports."+name+".format", format, ctx)
		if err != nil {
			return Export{}, err
		}
		parts = append(parts, s)
	}
	return Export{Value: strings.Join(parts, sep)}, nil
}

// evalFields 求值具名字段形态。
//
// 字段在**第一个实例**的上下文里求值：带凭据的连接点指向一个具体的服务
// （PG 的 primary），不是一个列表。
func (r *run) evalFields(
	name string, e pack.Export, head *instance, ctx *Ctx, port string,
) (Export, error) {
	decls := r.paramDecls(head.Role)
	out := Export{
		Fields:          map[string]string{},
		SensitiveFields: map[string]bool{},
	}

	for _, f := range e.FieldNames() {
		expr := e.Fields[f]
		ctx.Port = port
		v, err := r.eng.Expr(fmt.Sprintf("exports.%s.fields.%s", name, f), expr, ctx)
		if err != nil {
			return Export{}, err
		}
		out.Fields[f] = v

		// **敏感标记是推导出来的，不是声明的**：字段引用了 secret 参数，
		// 该字段就是敏感的。让 Pack 作者手工标注等于给了一个会被忘记的
		// 机会，而忘记的后果是口令随导出流进消费方的规格与归档。
		for _, p := range pack.ExprReferencedParams(expr) {
			if d, ok := decls[p]; ok && d.IsSensitive() {
				out.SensitiveFields[f] = true
				break
			}
		}
	}
	if len(out.SensitiveFields) == 0 {
		out.SensitiveFields = nil
	}
	return out, nil
}

// exportCtx 为一个实例构造导出求值用的上下文。
//
// 复制一份 Params：格式模板要往里塞 `.Port`，而直接改实例的上下文
// 会污染后续对它的任何求值。
func (r *run) exportCtx(it *instance, topo map[string]*TopologyCtx) *Ctx {
	c := *it.ctx
	c.Params = make(map[string]any, len(it.ctx.Params)+1)
	for k, v := range it.ctx.Params {
		c.Params[k] = v
	}
	c.Topology = topo[it.Key()]
	// `{{ .Address }}` 是 spec §5.4 里字段表达式的写法——在导出的语境下
	// 「地址」指的就是提供这个连接点的那台机器
	c.Address = it.Node.Address
	return &c
}

func instancesOfRole(insts []*instance, role string) []*instance {
	var out []*instance
	for _, it := range insts {
		if it.Role == role {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

func sortedExportNames(m map[string]pack.Export) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
