// Package state 管理 mechlet 的本地状态。
//
// 设计见 docs/design/12-spec-and-state.md。全部是 JSON 文件，写入方式为
// 「写临时文件 + rename」（POSIX 原子重命名），任何时刻崩溃都不会留下半个
// 文件（ADR-0013）。
//
// 范围界定：这里只存 mechlet **自身**的状态。事件与审计的权威存储在 mechd
// 的 SQLite 中；本包的事件缓冲只服务于断连期间，重连上报后即可删除。
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion 是状态文件结构的版本。
const SchemaVersion = 1

// 目录布局。
const (
	dirInstances = "instances"
	dirSpecs     = "specs"
	dirEvents    = "events"
	fileNode     = "node.json"
	fileFacts    = "facts.json"
)

// Store 是 mechlet 本地状态的读写入口。
type Store struct {
	// root 通常是 <data-dir>/mechlet。
	root string
}

// New 构造一个状态存储。root 不存在时创建。
func New(root string) (*Store, error) {
	for _, d := range []string{root, filepath.Join(root, dirInstances),
		filepath.Join(root, dirSpecs), filepath.Join(root, dirEvents)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("创建状态目录 %s: %w", d, err)
		}
	}
	return &Store{root: root}, nil
}

// Root 返回状态根目录。
func (s *Store) Root() string { return s.root }

// ── node.json ───────────────────────────────────────────────────────────

// Node 是节点身份与固化设置。
type Node struct {
	SchemaVersion int    `json:"schemaVersion"`
	NodeName      string `json:"nodeName"`
	NodeID        string `json:"nodeID"`
	// DataDir 是**固化值**：mechlet 启动时校验它与 --data-dir 一致，
	// 不一致则拒绝启动并报错，不做自动迁移。
	DataDir     string            `json:"dataDir"`
	Roots       map[string]string `json:"roots"`
	Volumes     []Volume          `json:"volumes,omitempty"`
	Upstream    string            `json:"upstream"`
	InstalledAt time.Time         `json:"installedAt"`
}

// Volume 是节点声明的一块盘。
type Volume struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Class string `json:"class,omitempty"`
}

// LoadNode 读取节点信息；文件不存在时返回 (nil, nil)。
func (s *Store) LoadNode() (*Node, error) {
	var n Node
	ok, err := readJSON(filepath.Join(s.root, fileNode), &n)
	if err != nil || !ok {
		return nil, err
	}
	return &n, nil
}

// SaveNode 原子写入节点信息。
func (s *Store) SaveNode(n *Node) error {
	n.SchemaVersion = SchemaVersion
	return writeJSON(filepath.Join(s.root, fileNode), n)
}

// CheckDataDir 校验 data-dir 与固化值一致。
//
// 不一致时拒绝而非自动迁移——迁移数据目录是运维动作，工具不该替用户决定。
func (n *Node) CheckDataDir(want string) error {
	if n.DataDir == "" || n.DataDir == want {
		return nil
	}
	return fmt.Errorf(
		"data-dir 与已固化值不一致\n"+
			"  已固化: %s\n"+
			"  本次给定: %s\n"+
			"  迁移数据目录是运维动作，请手工迁移后再启动",
		n.DataDir, want)
}

// ── instances/<component>__<role>.json ──────────────────────────────────

// GenState 是 generation 的生命周期状态。
type GenState string

const (
	// GenActive 表示 current 软链指向它。
	GenActive GenState = "active"
	// GenRetained 表示保留供回滚。
	GenRetained GenState = "retained"
	// GenFailed 表示物化或健康检查失败，保留供诊断，不参与回滚。
	GenFailed GenState = "failed"
)

// Generation 是一次完整物化的记录。
type Generation struct {
	Seq int `json:"seq"`
	// Digest 是该 generation 对应规格的摘要——generation 的真实身份。
	// 目录名只是给人看的。
	Digest         string    `json:"digest"`
	Version        string    `json:"version"`
	Revision       int       `json:"revision"`
	Dir            string    `json:"dir"`
	MaterializedAt time.Time `json:"materializedAt"`
	State          GenState  `json:"state"`
	// Note 记录失败原因等诊断信息。
	Note string `json:"note,omitempty"`

	// Blobs 是这一代引用的载荷摘要。
	//
	// 它可以从规格算出来，仍然记在这里，是为了与 Images 用同一条判据：
	// 一代被回收时，规格也随目录没了。
	Blobs []string `json:"blobs,omitempty"`
	// Images 是这一代物化出来的镜像引用。
	//
	// **只有物化那一刻才知道**：镜像引用来自 `docker load` 的输出，
	// 不是规格的函数（22-upgrade §2.5 ①）。
	Images []string `json:"images,omitempty"`
}

// ResourceRef 是一条已应用资源的身份，用于「已不再声明但仍存在」的比对。
type ResourceRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// ReconcileRecord 是最近一次调和的摘要。
type ReconcileRecord struct {
	At         time.Time `json:"at"`
	Result     string    `json:"result"` // ok | failed | skipped
	DurationMs int64     `json:"durationMs"`
	Message    string    `json:"message,omitempty"`
}

// Instance 是一个 RoleInstance 的本地状态。
type Instance struct {
	SchemaVersion int    `json:"schemaVersion"`
	Component     string `json:"component"`
	Role          string `json:"role"`
	ConfigGroup   string `json:"configGroup"`

	// Paths 是**固化的**路径。每次调和都用规格中的路径与它比对，
	// 不一致则拒绝调和——否则用户改了 roots 或 Pack 改了默认路径，
	// 已装组件会静默搬家，旧数据变成孤儿。
	Paths map[string][]string `json:"paths"`

	CurrentGeneration int          `json:"currentGeneration"`
	Generations       []Generation `json:"generations"`

	// AppliedResources 是上次实际应用过的资源身份。本次规格中不再出现的，
	// 引擎不自动清理，而是上报——多数资源没有可靠的「反向」。
	AppliedResources []ResourceRef `json:"appliedResources,omitempty"`

	// Digests 缓存文件的 (size, mtime) → sha256，免掉每轮的全量哈希。
	// 见 resource.DigestCache 的取舍说明。
	Digests map[string]DigestEntry `json:"digests,omitempty"`

	LastReconcile *ReconcileRecord `json:"lastReconcile,omitempty"`

	// Removed 记录这个实例已被卸载，以及卸载时**留下了什么**。
	//
	// 非 nil 时这条记录不再是一个「装着的实例」，而是一张收据：
	// Generations / AppliedResources / Digests 都已清空，机器上属于它的
	// 只剩 RetainedPaths 里那些目录。
	//
	// **不能卸载完就把状态文件删掉。** 数据目录默认保留（10-cli §4.3），
	// 而保留却不留下任何记录，等于在机器上制造一堆没人知道来历的目录——
	// 「保留而不提供发现机制等于把问题推给未来」。`orphans` 就是靠这条
	// 记录才列得出节点、路径与大小。
	//
	// 什么都没保留时**不写这条记录**，整个状态文件直接删掉：
	// 一次干净的卸载不该在机器上留下任何痕迹，包括这张收据。
	Removed *Removal `json:"removed,omitempty"`

	// LastWorkload 记录**最近一次**对工作负载的纠正动作。
	//
	// 记时间戳而不是「本轮做了什么」，是因为后者是**事件**而上报是
	// **周期快照**：调和比上报密的时候，那一轮几乎必然在被上报之前就被
	// 下一轮覆盖掉。规格里对 digest 写过同一句话——状态可以重复确认，
	// 事件丢一次就永远丢了。
	LastWorkload *WorkloadEvent `json:"lastWorkload,omitempty"`
}

// Removal 是一次卸载留下的收据。
type Removal struct {
	At time.Time `json:"at"`
	// Pack / Version 是被卸载的那一版，供 `orphans list` 说明来历。
	//
	// **必须记下来**：一个只有路径的孤儿回答不了「这堆数据是谁的、
	// 哪个版本写的」，而那正是决定「能不能删」时唯一要紧的问题。
	Pack    string `json:"pack,omitempty"`
	Version string `json:"version,omitempty"`
	// RetainedPaths 是保留下来的目录，按路径字典序，已去重。
	RetainedPaths []string `json:"retainedPaths,omitempty"`
}

// WorkloadEvent 是一次对工作负载的纠正。
type WorkloadEvent struct {
	// Action 取 restored | stopped。
	Action string    `json:"action"`
	At     time.Time `json:"at"`
}

// DigestEntry 是一条摘要缓存。
type DigestEntry struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	SHA256  string    `json:"sha256"`
	// CachedAt 是记录这条摘要的时刻。mtime 不严格早于它的条目一律
	// 当作脏的——见 resource.DigestUsable 的 racily clean 说明。
	CachedAt time.Time `json:"cachedAt"`
}

// InstanceKey 返回实例在状态目录中的键。
func InstanceKey(component, role string) string { return component + "__" + role }

func (s *Store) instancePath(key string) string {
	return filepath.Join(s.root, dirInstances, key+".json")
}

// LoadInstance 读取实例状态；不存在时返回 (nil, nil)。
func (s *Store) LoadInstance(key string) (*Instance, error) {
	var in Instance
	ok, err := readJSON(s.instancePath(key), &in)
	if err != nil || !ok {
		return nil, err
	}
	return &in, nil
}

// SaveInstance 原子写入实例状态。
func (s *Store) SaveInstance(in *Instance) error {
	in.SchemaVersion = SchemaVersion
	key := InstanceKey(in.Component, in.Role)
	return writeJSON(s.instancePath(key), in)
}

// DeleteInstance 移除实例状态及其保留的规格。
func (s *Store) DeleteInstance(key string) error {
	if err := os.Remove(s.instancePath(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.root, dirSpecs, key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListInstances 列出全部实例键，按字典序。
func (s *Store) ListInstances() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dirInstances))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(out)
	return out, nil
}

// ── generation 台账操作 ─────────────────────────────────────────────────

// FindGeneration 按 digest 查找已有的 generation。
//
// 命中意味着可以**直接切软链**而不必重新物化——这是回滚能秒级完成的原因。
func (in *Instance) FindGeneration(digest string) *Generation {
	for i := range in.Generations {
		if in.Generations[i].Digest == digest {
			return &in.Generations[i]
		}
	}
	return nil
}

// Active 返回当前生效的 generation；没有则返回 nil。
func (in *Instance) Active() *Generation {
	for i := range in.Generations {
		if in.Generations[i].Seq == in.CurrentGeneration {
			return &in.Generations[i]
		}
	}
	return nil
}

// NextSeq 返回下一个可用的 generation 序号。
func (in *Instance) NextSeq() int {
	max := 0
	for _, g := range in.Generations {
		if g.Seq > max {
			max = g.Seq
		}
	}
	return max + 1
}

// AddGeneration 追加一条 generation 记录，并按 seq 倒序排列。
func (in *Instance) AddGeneration(g Generation) {
	in.Generations = append(in.Generations, g)
	sort.Slice(in.Generations, func(i, j int) bool {
		return in.Generations[i].Seq > in.Generations[j].Seq
	})
}

// SetActive 把指定 seq 标记为 active，其余 active 降级为 retained。
func (in *Instance) SetActive(seq int) {
	for i := range in.Generations {
		switch {
		case in.Generations[i].Seq == seq:
			in.Generations[i].State = GenActive
		case in.Generations[i].State == GenActive:
			in.Generations[i].State = GenRetained
		}
	}
	in.CurrentGeneration = seq
}

// Prunable 返回超出保留数、可以回收的 generation。
//
// **只在新 generation 通过健康检查之后调用**——在那之前不能删任何东西，
// 否则回滚就没有落脚点了。
//
// active 永不回收；failed 的单独计数保留，供诊断。
func (in *Instance) Prunable(retain int) []Generation {
	if retain <= 0 {
		retain = 3
	}
	var healthy, failed []Generation
	for _, g := range in.Generations {
		switch g.State {
		case GenActive:
			continue
		case GenFailed:
			failed = append(failed, g)
		default:
			healthy = append(healthy, g)
		}
	}
	// Generations 已按 seq 倒序，healthy 也是
	var out []Generation
	if len(healthy) > retain-1 { // -1 是给 active 留的位置
		out = append(out, healthy[retain-1:]...)
	}
	// failed 只保留最近一个
	if len(failed) > 1 {
		out = append(out, failed[1:]...)
	}
	return out
}

// RemoveGeneration 从台账中移除指定 seq。
func (in *Instance) RemoveGeneration(seq int) {
	out := in.Generations[:0]
	for _, g := range in.Generations {
		if g.Seq != seq {
			out = append(out, g)
		}
	}
	in.Generations = out
}

// ── 路径固化校验 ────────────────────────────────────────────────────────

// CheckPaths 校验规格中的路径与已固化值一致。
//
// 不一致时拒绝调和而非自动迁移：若每次调和都重新推导，用户改了
// Node.Roots.data、或 Pack 升级时改了默认路径，已安装的组件会**静默搬家**
// ——新路径下是空目录，旧数据变成无人认领的孤儿。
func (in *Instance) CheckPaths(want map[string][]string) error {
	if len(in.Paths) == 0 {
		return nil // 首次物化，尚未固化
	}
	var problems []string
	for name, pinned := range in.Paths {
		got, ok := want[name]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"  paths.%s  已固化: %s\n              本次规格中不存在", name, join(pinned)))
			continue
		}
		if !equalSlices(pinned, got) {
			problems = append(problems, fmt.Sprintf(
				"  paths.%s  已固化: %s\n              本次解析: %s",
				name, join(pinned), join(got)))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s/%s 路径变更被拒绝\n%s\n  迁移数据目录是运维动作，"+
		"请手工迁移后使用 mechctl component adopt-path 更新记录",
		in.Component, in.Role, strings.Join(problems, "\n"))
}

func join(ss []string) string { return strings.Join(ss, ", ") }

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
