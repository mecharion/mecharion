package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
)

// ErrReloadUnsupported 表示该工作负载没有声明 execReload。
//
// 调用方应当降级为 restart，而不是当成失败——「这个组件不支持热加载」
// 是 Pack 的事实，不是运行期故障。
var ErrReloadUnsupported = errors.New("该工作负载未声明 execReload，不支持热加载")

// ErrUnavailable 表示本机没有这个 Runtime。
var ErrUnavailable = errors.New("本机不支持该 Runtime")

// ── 注册表 ──────────────────────────────────────────────────────────────

// Registry 按名字持有可用的 Runtime。
//
// mechlet 启动时注册它编译进来的全部实现，调和器按 workload.runtime
// 取用。这就是 05-runtime §1 说的「把分支收敛到一个地方」。
type Registry struct {
	byName map[string]Runtime
}

// NewRegistry 构造注册表。
func NewRegistry(rs ...Runtime) *Registry {
	reg := &Registry{byName: make(map[string]Runtime, len(rs))}
	for _, r := range rs {
		reg.Register(r)
	}
	return reg
}

// Register 登记一个 Runtime。同名后注册的覆盖先注册的。
func (r *Registry) Register(rt Runtime) {
	if r.byName == nil {
		r.byName = map[string]Runtime{}
	}
	r.byName[rt.Name()] = rt
}

// Get 按名字取 Runtime。
func (r *Registry) Get(name string) (Runtime, error) {
	rt, ok := r.byName[name]
	if !ok {
		return nil, faults.Permanentf("选择 Runtime",
			"未知的 runtime %q（本版本支持：%s）", name, strings.Join(r.Names(), ", "))
	}
	return rt, nil
}

// Reclaimers 返回实现了 ImageReclaimer 的 Runtime，按名字字典序。
//
// 回收要问过**每一个**管镜像的 Runtime：docker 与 compose 共用同一个本地
// 镜像库，删哪一个都一样，但这件事是它们的实现细节而不是这里的假设——
// 将来若有一个自带镜像库的 Runtime，这里不必改。
func (r *Registry) Reclaimers() []ImageReclaimer {
	var out []ImageReclaimer
	for _, name := range r.Names() {
		if rc, ok := r.byName[name].(ImageReclaimer); ok {
			out = append(out, rc)
		}
	}
	return out
}

// Names 返回已注册的 Runtime 名，按字典序。
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for k := range r.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// For 取出一份工作负载声明所需的 Runtime。
func (r *Registry) For(w *spec.Workload) (Runtime, error) {
	if w == nil {
		return nil, faults.Permanentf("选择 Runtime", "工作负载为空")
	}
	return r.Get(w.Runtime)
}

// Probe 探测全部已注册的 Runtime，供 Node capability 上报。
func (r *Registry) Probe(ctx context.Context) map[string]Capability {
	out := make(map[string]Capability, len(r.byName))
	for name, rt := range r.byName {
		cap, err := rt.Probe(ctx)
		if err != nil {
			cap = Capability{Available: false, Reason: fmt.Sprintf("探测失败: %v", err)}
		}
		out[name] = cap
	}
	return out
}
