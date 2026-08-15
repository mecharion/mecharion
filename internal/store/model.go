package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

// 领域类型。它们与 sqlcgen 的行结构**刻意分开**：后者的字段全是
// string / int64（SQLite 没有原生的时间、JSON、可空整型），直接把它端到
// 业务层，每个调用点都要自己解 JSON、自己 parse 时间——那种重复迟早会
// 出现两种解法。转换只在本文件里发生一次。

// Site 是一个站点。
type Site struct {
	ID        int64
	Name      string
	Kind      string
	Labels    map[string]string
	CreatedAt time.Time
}

// Volume 是节点上的一块盘。
type Volume struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Class string `json:"class,omitempty"`
}

// Node 是一台被管理的机器。
type Node struct {
	ID      int64
	SiteID  int64
	Name    string
	Address string
	Labels  map[string]string
	// Roots 是路径根（opt / etc / data / logs / run）。
	Roots   map[string]string
	Volumes []Volume
	// Status 答两个问题：**有没有发过证书、出现过没有**
	// （NodeReserved / NodePending / NodeSeen）。
	//
	// 它刻意不表达「在不在线」。在线是一件会过期的事，而这里存的是
	// 不会过期的事实——写进去之后再没有人需要把它写回去，也就没有
	// 「忘了写回去」这种失败模式（22-multi-node §6.13）。
	Status    string
	CreatedAt time.Time
	// RevokedAt 非空表示这台机器的证书已被吊销：握手仍会成功，
	// 但任何 RPC 都被拒（ADR-0034 的应用层吊销）。
	RevokedAt *time.Time
	// CordonedAt 非空表示**暂停调和**：连接与上报照常，只是不再把
	// 期望状态往机器上落。与吊销是两件完全不同的事。
	CordonedAt *time.Time
}

// nodes.status 的三个值。
const (
	// NodeReserved：node add 预登记，还没有人对它执行过 join，
	// 没有签发过任何证书（22-multi-node §6.16）。
	NodeReserved = "reserved"
	// NodePending：已经拿到证书（join 或 standalone 安装），
	// 但那台机器还没有连上来过。
	NodePending = "pending"
	// NodeSeen：连上来过。**一旦置上就不再变回去。**
	NodeSeen = "seen"
)

// Reserved 报告这一行是不是「预登记、还没发过证书」——
// Join 认领 reserved 行与插入全新行走的是同一条安全边界（ClaimNode）。
func (n Node) Reserved() bool { return n.Status == NodeReserved }

// Seen 报告这台机器有没有连上来过。
func (n Node) Seen() bool { return n.Status == NodeSeen }

// Revoked 报告这个节点是否已被吊销。
func (n Node) Revoked() bool { return n.RevokedAt != nil }

// Cordoned 报告这个节点是否被暂停调和。
func (n Node) Cordoned() bool { return n.CordonedAt != nil }

// PackRef 标识一个 Pack 的具体版本。
type PackRef struct {
	Name     string
	Version  string
	Revision int
}

// Component 是一次部署的期望状态。
type Component struct {
	ID      int64
	SiteID  int64
	Name    string
	Pack    PackRef
	Profile string
	Params  map[string]any
	// DriftPolicy 是站点侧对 Pack 声明的覆盖，**只能放松不能收紧**
	// （06-state-and-drift §4.2）。空串表示不覆盖。
	DriftPolicy string
	// PreviousVersion / PreviousRevision 是上一个版本，`rollback` 不带参数时的目标。
	//
	// 只在版本真的变了时更新：同一版本重复 deploy 不该把它冲掉，
	// 否则一次无意义的重复操作会让回滚目标变成它自己。
	PreviousVersion  string
	PreviousRevision int
	// 滚动升级的两个旋钮（22-multi-node §2.4）。**没有 maxSurge**
	// （ADR-0035）：裸机实例有固化 ordinal、固定数据目录与端口。
	RolloutMaxUnavailable int
	// RolloutCanary 是首批大小；0 表示不做金丝雀。
	RolloutCanary int

	// State 是生命周期状态：active（默认）| removing（24-lifecycle §2.2）。
	//
	// **`removed` 只是期望，记录不能在下发那一刻就删。** 删了之后那个实例
	// 就不在下发里了，节点再也收不到「这个实例不该存在」——它会变成孤儿，
	// 而孤儿永不自动删。因此要在「已下发卸载意图」与「全部实例都拆完了」
	// 之间停留一段时间，`removing` 就是这段时间。
	State string
	// Removal 是卸载开关，只在 State==removing 时有意义。
	//
	// 落库而不是只在 remove 那一刻用一次：下发是每 60 秒一次的持续行为，
	// 每一轮都要把同样的开关带给节点。
	Removal ComponentRemoval
	// RemovingAt 是进入 removing 的时刻，空值表示不在 removing。
	//
	// 「它从什么时候开始卡的」决定了运维要不要动 `--force`，而一个停在
	// removing 的组件正是这条路上最常见的现场。
	RemovingAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Component 的生命周期状态取值。
const (
	// ComponentActive 是默认：正常在管的组件。
	ComponentActive = "active"
	// ComponentRemoving 表示卸载意图已下发，还在等实例报告拆完。
	ComponentRemoving = "removing"
)

// Removing 报告这个 Component 是否正在被移除。
//
// 判据是 State 而不是 RemovingAt 非零：时间戳是给人看的，状态才是给
// 代码看的。两个都能判会让某处写成判时间戳，而那一处迟早会遇到一条
// 时间戳没写好的记录。
func (c Component) Removing() bool { return c.State == ComponentRemoving }

// ComponentRemoval 是卸载时的处置开关，对应 10-cli §4.3 那张表。
//
// 三个都是「偏离默认」的形式，零值即安全默认：**配置删、数据留、
// 用户留**。它与 spec.Removal 一一对应，渲染时原样带下去。
type ComponentRemoval struct {
	KeepConfig bool
	PurgeData  bool
	PurgeUser  bool
}

// ConfigGroup 是共享同一份参数覆盖的、具名的 RoleInstance 子集。
type ConfigGroup struct {
	ID          int64
	ComponentID int64
	Role        string
	Name        string
	Members     []string
	Params      map[string]any
	// Paths 是按卷名做的多盘绑定：路径名 → 卷名列表（spec §8.6）。
	//
	// 例如 {"dataDirs": ["data1", "data2"]} 让这一组机器的 dataDirs
	// 落在 /data1 与 /data2 上——这正是 ADR-0021 的原始动机场景
	// （20 台 4 盘、5 台 12 盘的 DataNode）。
	Paths map[string][]string
}

// RoleInstance 是一个角色在一台机器上的实例。
type RoleInstance struct {
	ID          int64
	ComponentID int64
	Role        string
	NodeID      int64
	// Ordinal 一次分配后固化，**永不重算**（ADR-0028）。
	Ordinal int
	// ConfigGroupID 为 nil 表示不属于任何配置组。
	ConfigGroupID *int64
	// Facts 是**放置时**的节点事实快照，不是实时值（spec §9.4.1）。
	//
	// 解析管线按需重算，若读的是随心跳刷新的实时事实，一次加内存就会
	// 让 heap 变化、digest 变化、服务在业务时间被重启。
	Facts map[string]any
	// RunState 是期望运行态：running（默认）| stopped。
	//
	// 它不进 spec digest——停一个服务不改变盘上任何一个字节
	// （internal/spec/digest.go 记了完整理由）。
	RunState  string
	CreatedAt time.Time
}

// PackBinding 是一条已固化的依赖绑定。
type PackBinding struct {
	ID               int64
	ComponentID      int64
	RequireName      string
	BoundComponentID int64
	CreatedAt        time.Time
}

// Dependent 是一个依赖者：哪个 Component 通过哪个 require 槽位用着它。
type Dependent struct {
	Component string
	Require   string
}

// ── 转换 ────────────────────────────────────────────────────────────────

func siteFrom(r sqlcgen.Site) (Site, error) {
	labels, err := decodeMap[string](r.Labels, "sites.labels")
	if err != nil {
		return Site{}, err
	}
	at, err := ParseTime(r.CreatedAt)
	if err != nil {
		return Site{}, fmt.Errorf("store: parsing sites.created_at: %w", err)
	}
	return Site{ID: r.ID, Name: r.Name, Kind: r.Kind, Labels: labels, CreatedAt: at}, nil
}

func nodeFrom(r sqlcgen.Node) (Node, error) {
	labels, err := decodeMap[string](r.Labels, "nodes.labels")
	if err != nil {
		return Node{}, err
	}
	roots, err := decodeMap[string](r.Roots, "nodes.roots")
	if err != nil {
		return Node{}, err
	}
	var vols []Volume
	if err := decodeJSON(r.Volumes, &vols, "nodes.volumes"); err != nil {
		return Node{}, err
	}
	at, err := ParseTime(r.CreatedAt)
	if err != nil {
		return Node{}, fmt.Errorf("store: parsing nodes.created_at: %w", err)
	}
	out := Node{
		ID: r.ID, SiteID: r.SiteID, Name: r.Name, Address: r.Address,
		Labels: labels, Roots: roots, Volumes: vols,
		Status: r.Status, CreatedAt: at,
	}
	if r.RevokedAt.Valid {
		t, err := ParseTime(r.RevokedAt.String)
		if err != nil {
			return Node{}, fmt.Errorf("store: parsing nodes.revoked_at: %w", err)
		}
		out.RevokedAt = &t
	}
	if r.CordonedAt.Valid {
		t, err := ParseTime(r.CordonedAt.String)
		if err != nil {
			return Node{}, fmt.Errorf("store: parsing nodes.cordoned_at: %w", err)
		}
		out.CordonedAt = &t
	}
	return out, nil
}

func componentFrom(r sqlcgen.Component) (Component, error) {
	params, err := decodeMap[any](r.Params, "components.params")
	if err != nil {
		return Component{}, err
	}
	created, err := ParseTime(r.CreatedAt)
	if err != nil {
		return Component{}, fmt.Errorf("store: parsing components.created_at: %w", err)
	}
	updated, err := ParseTime(r.UpdatedAt)
	if err != nil {
		return Component{}, fmt.Errorf("store: parsing components.updated_at: %w", err)
	}
	removingAt, err := parseOptionalTime(r.RemovingAt, "components.removing_at")
	if err != nil {
		return Component{}, err
	}
	// **空串按 active 处理。** 迁移给了默认值，但一条手工插进去的行、
	// 或将来某个漏填的写入路径都可能留下空串——而把一个正常组件当成
	// 「正在删除」是这里唯一不可接受的读错方向。
	state := r.State
	if state == "" {
		state = ComponentActive
	}
	return Component{
		ID: r.ID, SiteID: r.SiteID, Name: r.Name,
		PreviousVersion: r.PreviousVersion, PreviousRevision: int(r.PreviousRevision),
		Pack: PackRef{
			Name: r.PackName, Version: r.PackVersion,
			Revision: int(r.PackRevision),
		},
		Profile: r.Profile, Params: params, DriftPolicy: r.DriftPolicy,
		RolloutMaxUnavailable: int(r.RolloutMaxUnavailable),
		RolloutCanary:         int(r.RolloutCanary),
		State:                 state,
		Removal: ComponentRemoval{
			KeepConfig: r.RemovalKeepConfig != 0,
			PurgeData:  r.RemovalPurgeData != 0,
			PurgeUser:  r.RemovalPurgeUser != 0,
		},
		RemovingAt: removingAt,
		CreatedAt:  created, UpdatedAt: updated,
	}, nil
}

// parseOptionalTime 解析一个可以为空的时间列。
//
// 空串是合法的「没有值」，不是错误：removing_at 在绝大多数行上都是空的。
func parseOptionalTime(s, col string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := ParseTime(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parsing %s: %w", col, err)
	}
	return t, nil
}

func configGroupFrom(r sqlcgen.ConfigGroup) (ConfigGroup, error) {
	var members []string
	if err := decodeJSON(r.Members, &members, "config_groups.members"); err != nil {
		return ConfigGroup{}, err
	}
	params, err := decodeMap[any](r.Params, "config_groups.params")
	if err != nil {
		return ConfigGroup{}, err
	}
	var paths map[string][]string
	if err := decodeJSON(r.Paths, &paths, "config_groups.paths"); err != nil {
		return ConfigGroup{}, err
	}
	return ConfigGroup{
		ID: r.ID, ComponentID: r.ComponentID, Role: r.Role, Name: r.Name,
		Members: members, Params: params, Paths: paths,
	}, nil
}

func instanceFrom(r sqlcgen.RoleInstance) (RoleInstance, error) {
	at, err := ParseTime(r.CreatedAt)
	if err != nil {
		return RoleInstance{}, fmt.Errorf("store: parsing role_instances.created_at: %w", err)
	}
	facts, err := decodeMap[any](r.Facts, "role_instances.facts")
	if err != nil {
		return RoleInstance{}, err
	}
	inst := RoleInstance{
		ID: r.ID, ComponentID: r.ComponentID, Role: r.Role,
		NodeID: r.NodeID, Ordinal: int(r.Ordinal), CreatedAt: at,
		Facts: facts, RunState: r.RunState,
	}
	if r.ConfigGroupID.Valid {
		id := r.ConfigGroupID.Int64
		inst.ConfigGroupID = &id
	}
	return inst, nil
}

func bindingFrom(r sqlcgen.PackBinding) (PackBinding, error) {
	at, err := ParseTime(r.CreatedAt)
	if err != nil {
		return PackBinding{}, fmt.Errorf("store: parsing pack_bindings.created_at: %w", err)
	}
	return PackBinding{
		ID: r.ID, ComponentID: r.ComponentID, RequireName: r.RequireName,
		BoundComponentID: r.BoundComponentID, CreatedAt: at,
	}, nil
}

// ── JSON 列 ─────────────────────────────────────────────────────────────

func decodeMap[T any](raw, field string) (map[string]T, error) {
	m := map[string]T{}
	if err := decodeJSON(raw, &m, field); err != nil {
		return nil, err
	}
	return m, nil
}

func decodeJSON(raw string, v any, field string) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("store: parsing %s's JSON: %w", field, err)
	}
	return nil
}

// encodeJSON 把值编码为 JSON 列。nil 编为空对象/空数组而非 "null"——
// SQL 里对 "null" 字符串做判断是个坑，不如让列永远是合法的容器。
func encodeJSON(v any, empty string) (string, error) {
	if v == nil {
		return empty, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("store: encoding JSON column: %w", err)
	}
	if string(b) == "null" {
		return empty, nil
	}
	return string(b), nil
}
