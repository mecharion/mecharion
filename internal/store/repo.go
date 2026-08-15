package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/ident"
	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

// ErrNotFound 是所有仓储「查无此项」的统一返回。
//
// 不直接把 sql.ErrNoRows 端出去：那会让业务层依赖具体的存储实现，
// 而 07-persistence §1.5 要求存储访问走接口——为将来换实现留的唯一抽象。
var ErrNotFound = errors.New("store: not found")

// ErrTokenUnavailable 是 ClaimNode 在提交那一刻发现 token 已经不可用——
// 用尽、过期、被吊销，或被并发的另一个请求抢先用掉。三种原因外部看来
// 是同一件事：这张 token 现在不管用了。
var ErrTokenUnavailable = errors.New("store: join token unavailable")

// ErrNodeTaken 是 ClaimNode 在提交那一刻发现节点名已经在册、且不能被
// 认领——不管是被并发的另一个 Join 抢先注册、原本就有一行已发证书
// （pending/seen），还是一行被吊销的 reserved（预登记之后又被吊销，
// 不能再被认领）。
var ErrNodeTaken = errors.New("store: node name already registered")

// Repos 是全部领域仓储的入口。
type Repos interface {
	Sites() SiteRepo
	Nodes() NodeRepo
	Components() ComponentRepo
	ConfigGroups() ConfigGroupRepo
	Instances() InstanceRepo
	Bindings() BindingRepo

	// ── 观测侧：mechlet 报上来的东西 ──
	Status() StatusRepo
	Suppressions() SuppressionRepo
	Rollouts() RolloutRepo
	RolloutBatches() RolloutBatchRepo
	Events() EventRepo

	// JoinTokens 是节点加入用的一次性凭据（M7）。
	JoinTokens() JoinTokenRepo
	// Users 是 Web UI 的登录用户（M8）。
	Users() UserRepo
	// Sessions 是 Web UI 的登录会话（M8）。
	Sessions() SessionRepo

	// ClaimNode 原子地消耗一次 join token 的使用次数、并落库一行 Node，
	// 两步必须在同一个事务里，否则并发请求可能各自新建同名记录。名字
	// 不存在时新建；名字已存在且是 reserved（node add 预登记、未发过
	// 证书）时原地认领；其余情况一律拒绝。见实现注释。
	ClaimNode(ctx context.Context, tokenID int64, n Node, now time.Time) (Node, error)
}

// Session 是一次已登录的会话。
//
// **只存 token 的 sha256**，不存 token 本身：库被拖走时不能直接拿去冒充。
type Session struct {
	TokenHash string
	UserName  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionRepo 管理登录会话。
//
// 会话**落库**而不是放内存：放内存的话 mechd 一次重启就把所有人登出，
// 而重启在这个项目里是常规操作。
type SessionRepo interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, hash string) (Session, error)
	Delete(ctx context.Context, hash string) error
	// DeleteOfUser 清掉某人全部会话——改口令之后要用它。
	DeleteOfUser(ctx context.Context, name string) error
	DeleteExpired(ctx context.Context, before time.Time) error
}

// User 是一个能登录 Web UI 的人。
//
// **没有角色字段**，这是刻意的（ADR-0037）：现阶段登录即全权。
type User struct {
	ID   int64
	Name string
	// PasswordHash 是 argon2id 编码串，参数写在串里。
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserRepo 管理登录用户。
type UserRepo interface {
	Create(ctx context.Context, name, hash string, at time.Time) (User, error)
	GetByName(ctx context.Context, name string) (User, error)
	List(ctx context.Context) ([]User, error)
	// Count 用来回答「有没有用户」——登录页要靠它说清「先去建一个」。
	Count(ctx context.Context) (int, error)
	SetPassword(ctx context.Context, name, hash string, at time.Time) error
	Delete(ctx context.Context, name string) error
}

// SiteRepo 管理站点。
type SiteRepo interface {
	Create(ctx context.Context, s Site) (Site, error)
	Get(ctx context.Context, id int64) (Site, error)
	GetByName(ctx context.Context, name string) (Site, error)
	List(ctx context.Context) ([]Site, error)
}

// NodeRepo 管理节点。
type NodeRepo interface {
	Upsert(ctx context.Context, n Node) (Node, error)
	Get(ctx context.Context, id int64) (Node, error)
	GetByName(ctx context.Context, siteID int64, name string) (Node, error)
	List(ctx context.Context, siteID int64) ([]Node, error)
	SetStatus(ctx context.Context, id int64, status string) error
	// Delete 把一个节点从册子上抹掉。
	//
	// **不级联删实例**：那是调用方的决定，而「删节点顺手删掉它上面的
	// 组件」是一个没人预期的破坏性动作。
	Delete(ctx context.Context, id int64) error
	// InstancesOn 列出落在某节点上的实例，供删除前的检查。
	InstancesOn(ctx context.Context, nodeID int64) ([]RoleInstance, error)
	// SetRevoked 吊销或恢复一个节点的证书。at 为 nil 表示恢复。
	SetRevoked(ctx context.Context, id int64, at *time.Time) error
	// SetCordoned 暂停或恢复一个节点的调和。at 为 nil 表示恢复。
	SetCordoned(ctx context.Context, id int64, at *time.Time) error
}

// ComponentRepo 管理组件。
type ComponentRepo interface {
	// SetRollout 改滚动升级的两个旋钮。
	SetRollout(ctx context.Context, id int64, maxUnavailable, canary int, at time.Time) error
	Create(ctx context.Context, c Component) (Component, error)
	Get(ctx context.Context, id int64) (Component, error)
	GetByName(ctx context.Context, siteID int64, name string) (Component, error)
	List(ctx context.Context, siteID int64) ([]Component, error)
	Update(ctx context.Context, c Component) (Component, error)
	// StartRemoval 把 Component 置为 removing 并记下卸载开关。
	StartRemoval(ctx context.Context, id int64, rm ComponentRemoval, at time.Time) error
	Delete(ctx context.Context, id int64) error
}

// ConfigGroupRepo 管理配置组。
type ConfigGroupRepo interface {
	Upsert(ctx context.Context, g ConfigGroup) (ConfigGroup, error)
	List(ctx context.Context, componentID int64) ([]ConfigGroup, error)
	Get(ctx context.Context, componentID int64, role, name string) (ConfigGroup, error)
	Delete(ctx context.Context, componentID int64, role, name string) error
}

// InstanceRepo 管理角色实例。
type InstanceRepo interface {
	List(ctx context.Context, componentID int64) ([]RoleInstance, error)
	ListByRole(ctx context.Context, componentID int64, role string) ([]RoleInstance, error)

	// Ensure 返回 (component, role, node) 对应的实例：已存在则**原样返回**，
	// 不存在则分配一个新序号。
	//
	// 分配与插入在**同一个事务**里完成。拆成「先查最大值、再插入」会有
	// 竞态：两个并发的放置请求可能拿到同一个序号，而 ordinal 一旦写错
	// 就是集群身份错乱（ADR-0028）。
	Ensure(ctx context.Context, componentID int64, role string, nodeID int64,
		configGroupID *int64) (RoleInstance, error)

	// SetFacts 更新放置时的事实快照。
	SetFacts(ctx context.Context, id int64, facts map[string]any) error

	// SetRunState 设置一个实例的期望运行态。
	SetRunState(ctx context.Context, id int64, state string) error
	// SetComponentRunState 设置一个 Component 全部实例的期望运行态，
	// 返回受影响的行数。
	SetComponentRunState(ctx context.Context, componentID int64, state string) (int64, error)

	Delete(ctx context.Context, id int64) error
}

// BindingRepo 管理依赖绑定。
type BindingRepo interface {
	Get(ctx context.Context, componentID int64, requireName string) (PackBinding, error)
	List(ctx context.Context, componentID int64) ([]PackBinding, error)
	Create(ctx context.Context, b PackBinding) (PackBinding, error)
	// CountDependents 是删除保护：被依赖的 Component 在有依赖者时拒绝删除。
	CountDependents(ctx context.Context, componentID int64) (int, error)
	// ListDependents 列出依赖者，供拒绝时说清是哪几个。
	ListDependents(ctx context.Context, componentID int64) ([]Dependent, error)
}

// ── 实现 ────────────────────────────────────────────────────────────────

// Repos 返回仓储入口。
func (s *Store) Repos() Repos { return &repos{s: s} }

type repos struct{ s *Store }

func (r *repos) Sites() SiteRepo               { return &siteRepo{s: r.s} }
func (r *repos) Nodes() NodeRepo               { return &nodeRepo{s: r.s} }
func (r *repos) Components() ComponentRepo     { return &componentRepo{s: r.s} }
func (r *repos) ConfigGroups() ConfigGroupRepo { return &configGroupRepo{s: r.s} }
func (r *repos) Instances() InstanceRepo       { return &instanceRepo{s: r.s} }
func (r *repos) Bindings() BindingRepo         { return &bindingRepo{s: r.s} }

// ClaimNode 原子地：条件消耗一次 join token 的使用次数、并落库一行
// Node——名字不存在时新建，名字已存在且是 reserved（预登记未发证书）
// 时原地认领。两步在同一个事务里，任一失败则整体回滚。
//
// **并发认领的安全边界如下：** 此前 token 的可用性检查、节点存在性
// 检查、Node 落库（走的还是 Upsert，同名会静默覆盖）、token 计数增加
// 是四次独立的读写，中间的窗口能被并发请求同时钻进去：同一个 token
// 能签出两张有效证书，同一个节点
// 名的两次并发加入会让后一次悄悄覆盖前一次——两台物理机器最终都拿着
// CN 相同、被 CA 承认的有效证书，而吊销走的是「按节点名查状态」
// （ADR-0034），吊销这个名字会连带切断两台机器，无法只废掉冒充的那一张。
//
// **只有这一层是安全边界，`Service.Join` 里的存在性预检查不是**：那两次
// 检查只是为了在常见的非并发情况下给出更友好、指名道姓的错误，本身
// 完全不提供并发安全——过了不代表真的抢得到。
//
// **认领 reserved 行为什么是安全的**：reserved 行从未被签发过证书
// （22-multi-node §6.16），「两台机器拿到同一身份的两张有效证书」在这里
// 不成立——认领之前根本没有第一张证书可言。真正的
// 判据只有一个：`existing.Status == NodeReserved && !existing.Revoked()`；
// 判断与「原地把 reserved 改成 pending」在同一个事务里完成，中间不会
// 有别的写连接插进来（SQLite 单写连接模型），这与 `InstanceRepo.Ensure`
// 「先查再写」的既有模式是同一条纪律，不用一条取巧的 UPSERT+WHERE 换
// 一个更难在 Go 侧分辨「到底是不是真的认领成功了」的实现。
//
// 签发证书仍然在事务之外、先于这次调用（`Service.Join` 保持「先签发
// 再落库」的顺序不变）：一张签发了但没被用上的证书是无害的孤儿——它
// 对应的节点没有落库（或落库失败被回滚），任何 RPC 都会在身份收口处被拒
// （ADR-0034），也从未被返回给失败请求的调用方，不落盘、不外泄。
//
// **now 由调用方传入，不读 r.s.Now()**：token 是否过期是业务判断，
// 判断用的时钟应当与算出 ExpiresAt 时用的是同一个（`Service.now()`）；
// Store 自己的时钟只用于盖 CreatedAt 这类不参与判断的记账时间戳。
// 两者在测试夹具里是两个独立可替换的时钟——真发生过混用导致的假过期。
func (r *repos) ClaimNode(ctx context.Context, tokenID int64, n Node, now time.Time) (Node, error) {
	if err := ident.Validate(ident.Node, n.Name); err != nil {
		return Node{}, faults.Wrap(faults.Permanent, "加入集群", err)
	}
	labels, err := encodeJSON(n.Labels, "{}")
	if err != nil {
		return Node{}, err
	}
	roots, err := encodeJSON(n.Roots, "{}")
	if err != nil {
		return Node{}, err
	}
	vols, err := encodeJSON(n.Volumes, "[]")
	if err != nil {
		return Node{}, err
	}
	status := n.Status
	if status == "" {
		status = "unknown"
	}

	var out Node
	err = r.s.InTx(ctx, func(ctx context.Context) error {
		q := r.s.wq(ctx)

		affected, err := q.UseJoinTokenIfAvailable(ctx, sqlcgen.UseJoinTokenIfAvailableParams{
			ID: tokenID, ExpiresAt: FormatTime(now),
		})
		if err != nil {
			return fmt.Errorf("store: consuming join token: %w", err)
		}
		if affected == 0 {
			return ErrTokenUnavailable
		}

		existingRow, err := q.GetNodeByName(ctx, sqlcgen.GetNodeByNameParams{
			SiteID: n.SiteID, Name: n.Name,
		})
		switch {
		case errors.Is(err, sql.ErrNoRows):
			row, err := q.InsertNode(ctx, sqlcgen.InsertNodeParams{
				SiteID: n.SiteID, Name: n.Name, Address: n.Address,
				Labels: labels, Roots: roots, Volumes: vols, Status: status,
				CreatedAt: FormatTime(now),
			})
			if err != nil {
				if isUniqueViolation(err) {
					return ErrNodeTaken
				}
				return fmt.Errorf("store: registering Node %q: %w", n.Name, err)
			}
			out, err = nodeFrom(row)
			return err
		case err != nil:
			return fmt.Errorf("store: querying existing Node %q: %w", n.Name, err)
		default:
			existing, err := nodeFrom(existingRow)
			if err != nil {
				return err
			}
			if !existing.Reserved() || existing.Revoked() {
				return ErrNodeTaken
			}
			row, err := q.ClaimReservedNode(ctx, sqlcgen.ClaimReservedNodeParams{
				Address: n.Address, Status: status,
				SiteID: n.SiteID, Name: n.Name,
			})
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// 在我们的 SELECT 与这次 UPDATE 之间，这一行被
					// 另一个并发请求抢先认领掉了（同一个单写连接下
					// 理论上不会发生，这里只是防御性地给出正确错误，
					// 而不是让调用方看见一个 sql.ErrNoRows 的裸错误）。
					return ErrNodeTaken
				}
				return fmt.Errorf("store: claiming Node %q: %w", n.Name, err)
			}
			out, err = nodeFrom(row)
			return err
		}
	})
	if err != nil {
		return Node{}, err
	}
	return out, nil
}

var _ Repos = (*repos)(nil)

// txKey 是 context 里挂着的「当前事务」的键。
//
// Deploy 要把 Component、Instance、Facts、Bindings 的写入与
// 渲染串成一个 Unit of Work，任一环节失败都要整体回滚。走 context 传递
// 而不是给每个 repo 方法加一个 tx 参数——那会让 13 处渲染管线的调用点
// 都要跟着改签名，而这些调用点绝大多数根本不关心事务。ctx 里没有挂
// 事务时，wq/rq 照旧各走各的连接，行为与改动前完全一致。
type txKey struct{}

// WriteQueries 是 wq 面向包外的版本——`internal/vault` 与本包同样共用
// 一个 SQLite 数据库（Vault 自己直接 import 了 sqlcgen，两者本就不是
// 完全隔离的两个包），它的读写此前固定绑在 `Writer()` 上，与 Deploy 的
// 事务各自要一个写连接、而写连接只有一个：Deploy 持有事务
// 时渲染管线里生成新密钥会去问 Vault，Vault 再问连接池要一个新连接，
// 连接池只有一个还被 Deploy 自己攥着，永远等不到，直接卡死——曾在真实
// 超时下复现，goroutine dump 精确指到这里。让 Vault 也
// 认得 ctx 里的事务，跟 Deploy 共用同一个，而不是另开一个连接。
func (s *Store) WriteQueries(ctx context.Context) *sqlcgen.Queries { return s.wq(ctx) }

// wq 返回走写连接的 Querier；rq 返回走读连接池的。
// ctx 里挂着活跃事务时，两者都改用那个事务——同一个事务内的读要看得见
// 自己刚写的东西。
func (s *Store) wq(ctx context.Context) *sqlcgen.Queries {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return sqlcgen.New(tx)
	}
	return sqlcgen.New(s.write)
}
func (s *Store) rq(ctx context.Context) *sqlcgen.Queries {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return sqlcgen.New(tx)
	}
	return sqlcgen.New(s.read)
}

// notFound 把 sql.ErrNoRows 翻译成本包的哨兵。
func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

// sqliteConstraintUnique 是 SQLITE_CONSTRAINT_UNIQUE 的数值——
// modernc.org/sqlite 把它放在内部的 lib 子包，不在包顶层重新导出，
// 这里就近抄一份并注明来源，好过为一个常量去 import 内部包。
// 值本身是 SQLite 自己 C API 的一部分，稳定。
const sqliteConstraintUnique = 2067

// isUniqueViolation 判断错误是不是 UNIQUE 约束冲突。
//
// **靠类型判断，不靠字符串匹配错误信息**——`internal/mechd/httpapi.go`
// 的 `isUserError` 已经吃过这个亏：错误文案是给人看的，改起来不该
// 牵动控制流。
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
}

// ── sites ───────────────────────────────────────────────────────────────

type siteRepo struct{ s *Store }

// Create 建一个 Site。
//
// **这是全仓库唯一能落库 Site 的地方**（含 `mechlet install --standalone`
// 直接调用 repo 的路径），因此标识符校验放在这里而不是每个调用方各自
// 校验一遍——见 docs/design/09-naming-conventions.md §7。
func (r *siteRepo) Create(ctx context.Context, in Site) (Site, error) {
	if err := ident.Validate(ident.Site, in.Name); err != nil {
		return Site{}, faults.Wrap(faults.Permanent, "创建 Site", err)
	}
	labels, err := encodeJSON(in.Labels, "{}")
	if err != nil {
		return Site{}, err
	}
	row, err := r.s.wq(ctx).CreateSite(ctx, sqlcgen.CreateSiteParams{
		Name: in.Name, Kind: in.Kind, Labels: labels,
		CreatedAt: FormatTime(r.s.Now()),
	})
	if err != nil {
		return Site{}, fmt.Errorf("store: creating Site %q: %w", in.Name, err)
	}
	return siteFrom(row)
}

func (r *siteRepo) Get(ctx context.Context, id int64) (Site, error) {
	row, err := r.s.rq(ctx).GetSite(ctx, id)
	if err != nil {
		return Site{}, notFound(err, fmt.Sprintf("Site id=%d", id))
	}
	return siteFrom(row)
}

func (r *siteRepo) GetByName(ctx context.Context, name string) (Site, error) {
	row, err := r.s.rq(ctx).GetSiteByName(ctx, name)
	if err != nil {
		return Site{}, notFound(err, "Site "+name)
	}
	return siteFrom(row)
}

func (r *siteRepo) List(ctx context.Context) ([]Site, error) {
	rows, err := r.s.rq(ctx).ListSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing Sites: %w", err)
	}
	return mapRows(rows, siteFrom)
}

// ── nodes ───────────────────────────────────────────────────────────────

type nodeRepo struct{ s *Store }

// Upsert 建一个 Node，或对同名节点原样更新。
//
// **这是全仓库唯一能落库 Node 的地方**，同一份校验因此同时保护
// Service.AddNode、`mechlet install --standalone/--join` 与 Join 流程——
// 见 docs/design/09-naming-conventions.md §7。
func (r *nodeRepo) Upsert(ctx context.Context, in Node) (Node, error) {
	if err := ident.Validate(ident.Node, in.Name); err != nil {
		return Node{}, faults.Wrap(faults.Permanent, "写入 Node", err)
	}
	labels, err := encodeJSON(in.Labels, "{}")
	if err != nil {
		return Node{}, err
	}
	roots, err := encodeJSON(in.Roots, "{}")
	if err != nil {
		return Node{}, err
	}
	vols, err := encodeJSON(in.Volumes, "[]")
	if err != nil {
		return Node{}, err
	}
	status := in.Status
	if status == "" {
		status = "unknown"
	}
	row, err := r.s.wq(ctx).UpsertNode(ctx, sqlcgen.UpsertNodeParams{
		SiteID: in.SiteID, Name: in.Name, Address: in.Address,
		Labels: labels, Roots: roots, Volumes: vols, Status: status,
		CreatedAt: FormatTime(r.s.Now()),
	})
	if err != nil {
		return Node{}, fmt.Errorf("store: writing Node %q: %w", in.Name, err)
	}
	return nodeFrom(row)
}

func (r *nodeRepo) Get(ctx context.Context, id int64) (Node, error) {
	row, err := r.s.rq(ctx).GetNode(ctx, id)
	if err != nil {
		return Node{}, notFound(err, fmt.Sprintf("Node id=%d", id))
	}
	return nodeFrom(row)
}

func (r *nodeRepo) GetByName(ctx context.Context, siteID int64, name string) (Node, error) {
	row, err := r.s.rq(ctx).GetNodeByName(ctx, sqlcgen.GetNodeByNameParams{
		SiteID: siteID, Name: name,
	})
	if err != nil {
		return Node{}, notFound(err, "Node "+name)
	}
	return nodeFrom(row)
}

func (r *nodeRepo) List(ctx context.Context, siteID int64) ([]Node, error) {
	rows, err := r.s.rq(ctx).ListNodes(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: listing Nodes: %w", err)
	}
	return mapRows(rows, nodeFrom)
}

func (r *nodeRepo) SetRevoked(ctx context.Context, id int64, at *time.Time) error {
	var v sql.NullString
	if at != nil {
		v = sql.NullString{String: FormatTime(*at), Valid: true}
	}
	if err := r.s.wq(ctx).SetNodeRevoked(ctx, sqlcgen.SetNodeRevokedParams{
		RevokedAt: v, ID: id,
	}); err != nil {
		return fmt.Errorf("store: updating Node revocation status: %w", err)
	}
	return nil
}

func (r *nodeRepo) SetCordoned(ctx context.Context, id int64, at *time.Time) error {
	var v sql.NullString
	if at != nil {
		v = sql.NullString{String: FormatTime(*at), Valid: true}
	}
	if err := r.s.wq(ctx).SetNodeCordoned(ctx, sqlcgen.SetNodeCordonedParams{
		CordonedAt: v, ID: id,
	}); err != nil {
		return fmt.Errorf("store: updating Node cordon status: %w", err)
	}
	return nil
}

func (r *nodeRepo) Delete(ctx context.Context, id int64) error {
	if err := r.s.wq(ctx).DeleteNode(ctx, id); err != nil {
		return fmt.Errorf("store: deleting Node: %w", err)
	}
	return nil
}

func (r *nodeRepo) InstancesOn(ctx context.Context, nodeID int64) ([]RoleInstance, error) {
	rows, err := r.s.rq(ctx).ListRoleInstancesByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: listing instances on node: %w", err)
	}
	out := make([]RoleInstance, 0, len(rows))
	for _, row := range rows {
		v, err := instanceFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (r *nodeRepo) SetStatus(ctx context.Context, id int64, status string) error {
	if err := r.s.wq(ctx).SetNodeStatus(ctx, sqlcgen.SetNodeStatusParams{
		Status: status, ID: id,
	}); err != nil {
		return fmt.Errorf("store: updating Node status: %w", err)
	}
	return nil
}

// ── components ──────────────────────────────────────────────────────────

type componentRepo struct{ s *Store }

// Create 建一个 Component。
//
// **这是全仓库唯一能落库 Component 的地方**——见
// docs/design/09-naming-conventions.md §7。
func (r *componentRepo) Create(ctx context.Context, in Component) (Component, error) {
	if err := ident.Validate(ident.Component, in.Name); err != nil {
		return Component{}, faults.Wrap(faults.Permanent, "创建 Component", err)
	}
	params, err := encodeJSON(in.Params, "{}")
	if err != nil {
		return Component{}, err
	}
	now := FormatTime(r.s.Now())
	rev := int64(in.Pack.Revision)
	if rev == 0 {
		rev = 1
	}
	row, err := r.s.wq(ctx).CreateComponent(ctx, sqlcgen.CreateComponentParams{
		SiteID: in.SiteID, Name: in.Name,
		PackName: in.Pack.Name, PackVersion: in.Pack.Version, PackRevision: rev,
		Profile: in.Profile, Params: params, DriftPolicy: in.DriftPolicy,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Component{}, fmt.Errorf("store: creating Component %q: %w", in.Name, err)
	}
	return componentFrom(row)
}

func (r *componentRepo) Get(ctx context.Context, id int64) (Component, error) {
	row, err := r.s.rq(ctx).GetComponent(ctx, id)
	if err != nil {
		return Component{}, notFound(err, fmt.Sprintf("Component id=%d", id))
	}
	return componentFrom(row)
}

func (r *componentRepo) GetByName(ctx context.Context, siteID int64, name string) (Component, error) {
	row, err := r.s.rq(ctx).GetComponentByName(ctx, sqlcgen.GetComponentByNameParams{
		SiteID: siteID, Name: name,
	})
	if err != nil {
		return Component{}, notFound(err, "Component "+name)
	}
	return componentFrom(row)
}

func (r *componentRepo) List(ctx context.Context, siteID int64) ([]Component, error) {
	rows, err := r.s.rq(ctx).ListComponents(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: listing Components: %w", err)
	}
	return mapRows(rows, componentFrom)
}

func (r *componentRepo) Update(ctx context.Context, in Component) (Component, error) {
	params, err := encodeJSON(in.Params, "{}")
	if err != nil {
		return Component{}, err
	}
	rev := int64(in.Pack.Revision)
	if rev == 0 {
		rev = 1
	}
	row, err := r.s.wq(ctx).UpdateComponent(ctx, sqlcgen.UpdateComponentParams{
		PackVersion: in.Pack.Version, PackRevision: rev,
		Profile: in.Profile, Params: params, DriftPolicy: in.DriftPolicy,
		PreviousVersion: in.PreviousVersion, PreviousRevision: int64(in.PreviousRevision),
		UpdatedAt: FormatTime(r.s.Now()), ID: in.ID,
	})
	if err != nil {
		return Component{}, notFound(err, fmt.Sprintf("Component id=%d", in.ID))
	}
	return componentFrom(row)
}

func (r *componentRepo) SetRollout(
	ctx context.Context, id int64, maxUnavailable, canary int, at time.Time,
) error {
	err := r.s.wq(ctx).SetComponentRollout(ctx, sqlcgen.SetComponentRolloutParams{
		RolloutMaxUnavailable: int64(maxUnavailable),
		RolloutCanary:         int64(canary),
		UpdatedAt:             FormatTime(at), ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: updating Rollout knobs: %w", err)
	}
	return nil
}

// StartRemoval 把 Component 置为 removing，同时记下三个卸载开关。
//
// **一条 UPDATE 写完状态与开关**，不拆成两次。拆开会出现一个「已经在
// removing 但开关还是零值」的中间态，而下发是每 60 秒一次的持续行为——
// 它随时可能正好落在那一刻，于是一个 `--purge-data` 的移除会有一轮
// 按「保留数据」下发出去。那一轮里完成卸载的节点就把数据留下了，
// 而命令行明明说了要删。
func (r *componentRepo) StartRemoval(
	ctx context.Context, id int64, rm ComponentRemoval, at time.Time,
) error {
	err := r.s.wq(ctx).StartComponentRemoval(ctx, sqlcgen.StartComponentRemovalParams{
		// 状态走绑定参数而不是 SQL 里的字面量：sqlc 1.31 会把
		// `SET state = 'removing'` 里的引号**吃掉**，变成一个列引用，
		// 而且退出码是 0。症状要到运行期才现形（no such column: removing）。
		State:             ComponentRemoving,
		RemovalKeepConfig: boolToInt(rm.KeepConfig),
		RemovalPurgeData:  boolToInt(rm.PurgeData),
		RemovalPurgeUser:  boolToInt(rm.PurgeUser),
		RemovingAt:        FormatTime(at),
		UpdatedAt:         FormatTime(at), ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: setting to removing: %w", err)
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (r *componentRepo) Delete(ctx context.Context, id int64) error {
	if err := r.s.wq(ctx).DeleteComponent(ctx, id); err != nil {
		return fmt.Errorf("store: deleting Component: %w", err)
	}
	return nil
}

// ── config groups ───────────────────────────────────────────────────────

type configGroupRepo struct{ s *Store }

// Upsert 建一个 ConfigGroup，或对同名组原样更新。
//
// **这是全仓库唯一能落库 ConfigGroup 的地方**——见
// docs/design/09-naming-conventions.md §7。Role 不在这里校验：
// 它来自 Pack 定义，已由 `mechpack lint` 的 R09 强制同一条规则。
func (r *configGroupRepo) Upsert(ctx context.Context, in ConfigGroup) (ConfigGroup, error) {
	if err := ident.Validate(ident.ConfigGroup, in.Name); err != nil {
		return ConfigGroup{}, faults.Wrap(faults.Permanent, "写入配置组", err)
	}
	members, err := encodeJSON(in.Members, "[]")
	if err != nil {
		return ConfigGroup{}, err
	}
	params, err := encodeJSON(in.Params, "{}")
	if err != nil {
		return ConfigGroup{}, err
	}
	paths, err := encodeJSON(in.Paths, "{}")
	if err != nil {
		return ConfigGroup{}, err
	}
	row, err := r.s.wq(ctx).UpsertConfigGroup(ctx, sqlcgen.UpsertConfigGroupParams{
		ComponentID: in.ComponentID, Role: in.Role, Name: in.Name,
		Members: members, Params: params, Paths: paths,
	})
	if err != nil {
		return ConfigGroup{}, fmt.Errorf("store: writing config group %q: %w", in.Name, err)
	}
	return configGroupFrom(row)
}

func (r *configGroupRepo) List(ctx context.Context, componentID int64) ([]ConfigGroup, error) {
	rows, err := r.s.rq(ctx).ListConfigGroups(ctx, componentID)
	if err != nil {
		return nil, fmt.Errorf("store: listing config groups: %w", err)
	}
	return mapRows(rows, configGroupFrom)
}

func (r *configGroupRepo) Get(
	ctx context.Context, componentID int64, role, name string,
) (ConfigGroup, error) {
	row, err := r.s.rq(ctx).GetConfigGroup(ctx, sqlcgen.GetConfigGroupParams{
		ComponentID: componentID, Role: role, Name: name,
	})
	if err != nil {
		return ConfigGroup{}, notFound(err, "配置组 "+name)
	}
	return configGroupFrom(row)
}

func (r *configGroupRepo) Delete(
	ctx context.Context, componentID int64, role, name string,
) error {
	if err := r.s.wq(ctx).DeleteConfigGroup(ctx, sqlcgen.DeleteConfigGroupParams{
		ComponentID: componentID, Role: role, Name: name,
	}); err != nil {
		return fmt.Errorf("store: deleting config group %q: %w", name, err)
	}
	return nil
}

// ── role instances ──────────────────────────────────────────────────────

type instanceRepo struct{ s *Store }

func (r *instanceRepo) List(ctx context.Context, componentID int64) ([]RoleInstance, error) {
	rows, err := r.s.rq(ctx).ListRoleInstances(ctx, componentID)
	if err != nil {
		return nil, fmt.Errorf("store: listing instances: %w", err)
	}
	return mapRows(rows, instanceFrom)
}

func (r *instanceRepo) ListByRole(ctx context.Context, componentID int64, role string) ([]RoleInstance, error) {
	rows, err := r.s.rq(ctx).ListRoleInstancesByRole(ctx, sqlcgen.ListRoleInstancesByRoleParams{
		ComponentID: componentID, Role: role,
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing instances: %w", err)
	}
	return mapRows(rows, instanceFrom)
}

// Ensure 实现 ADR-0028 的分配规则。
func (r *instanceRepo) Ensure(
	ctx context.Context, componentID int64, role string, nodeID int64,
	configGroupID *int64,
) (RoleInstance, error) {
	var out RoleInstance

	err := r.s.InTx(ctx, func(ctx context.Context) error {
		q := r.s.wq(ctx)

		// 已存在就原样返回——**已有实例的序号永不改变**
		row, err := q.GetRoleInstance(ctx, sqlcgen.GetRoleInstanceParams{
			ComponentID: componentID, Role: role, NodeID: nodeID,
		})
		if err == nil {
			out, err = instanceFrom(row)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: querying instance: %w", err)
		}

		// 新实例取当前最大 + 1。**不用 count()**：序号允许空洞，
		// count 会撞上已经用过的号。
		maxOrd, err := q.MaxOrdinal(ctx, sqlcgen.MaxOrdinalParams{
			ComponentID: componentID, Role: role,
		})
		if err != nil {
			return fmt.Errorf("store: querying max ordinal: %w", err)
		}

		var cg sql.NullInt64
		if configGroupID != nil {
			cg = sql.NullInt64{Int64: *configGroupID, Valid: true}
		}
		created, err := q.CreateRoleInstance(ctx, sqlcgen.CreateRoleInstanceParams{
			ComponentID: componentID, Role: role, NodeID: nodeID,
			Ordinal: toInt64(maxOrd) + 1, ConfigGroupID: cg,
			CreatedAt: FormatTime(r.s.Now()),
		})
		if err != nil {
			return fmt.Errorf("store: creating instance: %w", err)
		}
		out, err = instanceFrom(created)
		return err
	})
	return out, err
}

func (r *instanceRepo) Delete(ctx context.Context, id int64) error {
	// 序号**不回收**：回收会让新实例拿到刚被释放的编号，而集群里其它成员的
	// 元数据可能还引用着旧成员（ADR-0028）。这里只删行，MaxOrdinal 因此
	// 仍然记得用过的最大值——除非删的正好是最大的那个。
	if err := r.s.wq(ctx).DeleteRoleInstance(ctx, id); err != nil {
		return fmt.Errorf("store: deleting instance: %w", err)
	}
	return nil
}

// ── bindings ────────────────────────────────────────────────────────────

type bindingRepo struct{ s *Store }

func (r *bindingRepo) Get(ctx context.Context, componentID int64, requireName string) (PackBinding, error) {
	row, err := r.s.rq(ctx).GetPackBinding(ctx, sqlcgen.GetPackBindingParams{
		ComponentID: componentID, RequireName: requireName,
	})
	if err != nil {
		return PackBinding{}, notFound(err, "绑定 "+requireName)
	}
	return bindingFrom(row)
}

func (r *bindingRepo) List(ctx context.Context, componentID int64) ([]PackBinding, error) {
	rows, err := r.s.rq(ctx).ListPackBindings(ctx, componentID)
	if err != nil {
		return nil, fmt.Errorf("store: listing bindings: %w", err)
	}
	return mapRows(rows, bindingFrom)
}

func (r *bindingRepo) Create(ctx context.Context, in PackBinding) (PackBinding, error) {
	row, err := r.s.wq(ctx).CreatePackBinding(ctx, sqlcgen.CreatePackBindingParams{
		ComponentID: in.ComponentID, RequireName: in.RequireName,
		BoundComponentID: in.BoundComponentID,
		CreatedAt:        FormatTime(r.s.Now()),
	})
	if err != nil {
		return PackBinding{}, fmt.Errorf("store: creating binding %q: %w", in.RequireName, err)
	}
	return bindingFrom(row)
}

// ListDependents 列出依赖这个 Component 的组件及其 require 槽位。
//
// **数字答不了唯一要紧的那个问题**：一次被拒绝的删除，人要知道的是
// 「是哪几个、我还需不需要它们」，而不是「有 3 个」。
func (r *bindingRepo) ListDependents(ctx context.Context, componentID int64) ([]Dependent, error) {
	rows, err := r.s.rq(ctx).ListDependents(ctx, componentID)
	if err != nil {
		return nil, fmt.Errorf("store: listing dependents: %w", err)
	}
	out := make([]Dependent, 0, len(rows))
	for _, row := range rows {
		out = append(out, Dependent{Component: row.Component, Require: row.RequireName})
	}
	return out, nil
}

func (r *bindingRepo) CountDependents(ctx context.Context, componentID int64) (int, error) {
	n, err := r.s.rq(ctx).CountDependents(ctx, componentID)
	if err != nil {
		return 0, fmt.Errorf("store: counting dependents: %w", err)
	}
	return int(n), nil
}

// ── 辅助 ────────────────────────────────────────────────────────────────

// mapRows 把一批行逐个转换，任一失败即整体失败。
func mapRows[R any, T any](rows []R, conv func(R) (T, error)) ([]T, error) {
	out := make([]T, 0, len(rows))
	for _, r := range rows {
		v, err := conv(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// toInt64 兼容 sqlc 对 COALESCE 结果的类型推断（可能是 int64 或 any）。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

// SetFacts 更新一个实例的**放置时事实快照**。
//
// 只在放置与 `node facts refresh --apply` 时调用，**不随心跳更新**：
// 配置取值跟随实时事实会把「运维觉察不到的事实变动」变成生产环境的
// 配置变更（spec §9.4.1）。
func (r *instanceRepo) SetFacts(ctx context.Context, id int64, facts map[string]any) error {
	blob, err := encodeJSON(facts, "{}")
	if err != nil {
		return err
	}
	return r.s.wq(ctx).SetRoleInstanceFacts(ctx, sqlcgen.SetRoleInstanceFactsParams{
		Facts: blob, ID: id,
	})
}

// SetRunState 设置一个实例的期望运行态。
func (r *instanceRepo) SetRunState(ctx context.Context, id int64, state string) error {
	return r.s.wq(ctx).SetRoleInstanceRunState(ctx, sqlcgen.SetRoleInstanceRunStateParams{
		RunState: state, ID: id,
	})
}

// SetComponentRunState 设置一个 Component 全部实例的期望运行态。
//
// 返回受影响的行数：**0 行是有意义的信息**——组件还没放置到任何节点上，
// 而一句「已停止」会让人以为真的停了什么。
func (r *instanceRepo) SetComponentRunState(
	ctx context.Context, componentID int64, state string,
) (int64, error) {
	return r.s.wq(ctx).SetComponentRunState(ctx, sqlcgen.SetComponentRunStateParams{
		RunState: state, ComponentID: componentID,
	})
}

// ── join token（M7）────────────────────────────────────────────────────

// JoinToken 是一张节点加入凭据。
//
// **明文只在创建那一刻存在**：库里存的是哈希，与 admin token 同一条纪律。
type JoinToken struct {
	ID     int64
	SiteID int64
	// Hash 是 token 明文的 sha256（十六进制）。
	Hash string
	// NodeName 非空表示这张 token 只能签出该名字的证书。
	//
	// 空表示不绑名——批量预置时名字先到先得，代价是拿到它的人可以往
	// 集群里塞一台机器（ADR-0034 里写明了这个取舍）。
	NodeName  string
	ExpiresAt time.Time
	MaxUses   int
	Used      int
	RevokedAt *time.Time
	CreatedAt time.Time
	CreatedBy string
}

// Usable 报告这张 token 现在还能不能用，不能时给出原因。
//
// 三种失效**分开报**：过期、用尽、已吊销是三件不同的事，而运维在现场
// 最先要知道的正是「是哪一种」——合成一句「token 无效」等于让他从头查起。
func (t JoinToken) Usable(now time.Time) (bool, string) {
	switch {
	case t.RevokedAt != nil:
		return false, "revoked"
	case now.After(t.ExpiresAt):
		return false, "expired (" + FormatTime(t.ExpiresAt) + ")"
	case t.Used >= t.MaxUses:
		return false, fmt.Sprintf("uses exhausted (%d/%d)", t.Used, t.MaxUses)
	}
	return true, ""
}

// JoinTokenRepo 管理加入凭据。
type JoinTokenRepo interface {
	Create(ctx context.Context, t JoinToken) (JoinToken, error)
	// GetByHash 按明文的哈希查——**没有按 id 查的入口**，
	// 因为校验路径上只有明文，而列表路径不需要单查。
	GetByHash(ctx context.Context, hash string) (JoinToken, error)
	List(ctx context.Context, siteID int64) ([]JoinToken, error)
	Revoke(ctx context.Context, id int64, at time.Time) error
}

// ── Rollout 批次（M7）──────────────────────────────────────────────────

// 批次状态。
const (
	// BatchPending 表示还没轮到它。
	BatchPending = "pending"
	// BatchReleased 表示已经下发，正在等它收敛。
	BatchReleased = "released"
	// BatchDone 表示这一批过了门禁。
	BatchDone = "done"
	// BatchFailed 表示这一批没过。
	BatchFailed = "failed"
	// BatchSkipped 表示这一批里没有可动的目标（例如全被 cordon 了）。
	BatchSkipped = "skipped"
)

// BatchTarget 是批次里的一个目标。
//
// **role / node 冗余存一份**：实例可能在 Rollout 之后被删掉，那时仅凭
// InstanceID 说不出它是谁——而历史记录的价值恰恰在事后。
type BatchTarget struct {
	InstanceID int64  `json:"instanceId"`
	Role       string `json:"role"`
	Node       string `json:"node"`
	Ordinal    int    `json:"ordinal"`
}

// RolloutBatch 是一次 Rollout 里的一批。
type RolloutBatch struct {
	ID        int64
	RolloutID int64
	// Seq 是全局序号（1 起），跨阶段连续——`rollout status` 要说
	// 「第 2/4 批」，那个分母是全局的。
	Seq int
	// Stage 是阶段序号，按角色的 requires 拓扑序。
	Stage   int
	Role    string
	Targets []BatchTarget
	State   string
	// ReleasedAt 是这一批被放行的时刻；batchTimeout 从它起算。
	//
	// **不从 Rollout 开始起算**：一次 4 批的升级合法地要花
	// 4×（物化 + 稳定窗口），拿一个全局超时去卡它必然误判。
	ReleasedAt time.Time
	// HealthySince 是这一批**首次**同时满足收敛与健康的时刻；零值表示
	// 窗口还没开始，或者被清掉了。
	//
	// 任何一个实例掉出判据就清空——那正是门禁挡住崩溃循环的全部机制：
	// 机器每崩一次就把它清掉，于是永远攒不满 stableFor。
	HealthySince time.Time
	// RestartBaseline 是窗口开始时各实例的工作负载重启次数。
	RestartBaseline map[int64]int
}

// RolloutBatchRepo 管理批次。
type RolloutBatchRepo interface {
	Create(ctx context.Context, b RolloutBatch) (RolloutBatch, error)
	List(ctx context.Context, rolloutID int64) ([]RolloutBatch, error)
	SetState(ctx context.Context, id int64, state string) error
	// Release 放行一批：状态与放行时刻**一起写**。
	//
	// 分两次写的话，中间崩一次就得到一个「已放行但不知道什么时候放行的」
	// 批次——batchTimeout 从此无从起算，那一批会永远等下去。
	Release(ctx context.Context, id int64, at time.Time) error
	// SetGate 刷新稳定窗口。since 为零值表示把窗口清掉。
	SetGate(ctx context.Context, id int64, since time.Time, baseline map[int64]int) error
}
