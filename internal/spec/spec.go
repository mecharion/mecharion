// Package spec 定义 ResolvedSpec —— mechd 下发给 mechlet 的已解析规格，
// 也是资源引擎的输入。
//
// 设计见 docs/design/12-spec-and-state.md。核心性质：
//
//   - 一份 spec 对应**一个 RoleInstance**，不是一个 Component、不是一个节点
//   - 除一个 late-bound 占位符外全部渲染完毕，mechlet 不做模板渲染
//   - mechlet 不需要（也无法）查询其他节点——拓扑是快照
package spec

import (
	"encoding/json"
	"time"
)

// SchemaVersion 是本结构自身的版本。
//
// 它独立于 Mecharion 的版本号。mechlet 收到更高版本时拒绝并上报自己支持的
// 版本；收到更低版本时按旧结构解析（保留至少两个大版本的兼容）。
const SchemaVersion = 1

// GenerationPlaceholder 是**唯一**允许残留在 spec 中的模板 token。
//
// generation 目录名含一个 mechlet 本地分配的序号（generations/0007-16.4-1），
// mechd 无从得知，因此只能留到物化时替换。
//
// 替换是**字面量替换，不是第二次模板渲染**——引入第二个渲染阶段会带来
// 「哪些变量在哪一阶段可用」的持续困惑。
const GenerationPlaceholder = "{{ .Paths.Generation }}"

// ResolvedSpec 是一个 RoleInstance 的完整期望状态。
type ResolvedSpec struct {
	SchemaVersion int `json:"schemaVersion"`
	// Digest 是规格内容的 sha256，是 generation 的身份。见 Compute。
	Digest string `json:"digest"`

	// ── 身份 ──
	Site        SiteRef `json:"site"`
	Component   string  `json:"component"`
	Role        string  `json:"role"`
	ConfigGroup string  `json:"configGroup"`
	Node        NodeRef `json:"node"`
	// Ordinal 是同角色内的稳定序号，按节点名排序（从 0 起）。
	Ordinal int `json:"ordinal"`

	// ── 来源 ──
	Pack    PackRef `json:"pack"`
	Profile string  `json:"profile,omitempty"`

	// ── 已解析的取值 ──
	Params map[string]ParamValue `json:"params,omitempty"`
	Paths  map[string]PathValue  `json:"paths"`

	// ── 载荷 ──
	Blobs []BlobRef `json:"blobs,omitempty"`

	// Resources 已按 shared 在前、role 在后排好序，且 when 已求值——
	// 求值为 false 的资源根本不在列表里。
	Resources []Resource `json:"resources"`

	Workload *Workload `json:"workload,omitempty"`
	Health   *Health   `json:"health,omitempty"`
	// Hooks 中 scope 与 when 都已被 mechd 求值：scope:once 已执行过的、
	// when 为 false 的都不下发。mechlet 收到就执行。
	Hooks []Hook `json:"hooks,omitempty"`

	// SecretRefs 是本规格引用的密钥，**只有 id 与版本号**。
	// 值随 gRPC 消息单独下发、不落盘，由 mechlet 在写盘最后一刻替换。
	//
	// 它**参与 digest**：见 ComputeDigest 的说明。
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`

	// Suppressions 是已生效的漂移抑制（06-state-and-drift §4.1）。
	//
	// **不参与 digest**（见 ComputeDigest）。
	Suppressions []Suppression `json:"suppressions,omitempty"`

	// RunState 是**期望的运行态**：running（默认）| stopped | removed。
	//
	// 它回答的是「这个服务现在应不应该在跑」，而这与「它应该长什么样」
	// 是两件事。有了它，`systemctl stop` 才有判据可依：没人说过话的停止
	// 是漂移，要恢复；`mechctl component stop` 之后的停止是意图，要维持
	// （20-continuous-reconcile §2.2）。
	//
	// **不参与 digest**（见 ComputeDigest）。
	RunState string `json:"runState,omitempty"`

	// Removal 是 RunState==removed 时的处置开关。其余运行态下无意义。
	//
	// **不参与 digest**（见 ComputeDigest）：它描述的是「拆的时候怎么拆」，
	// 不是「盘上应该是什么」。
	Removal *Removal `json:"removal,omitempty"`

	// ── 上下文 ──
	Topology Topology                  `json:"topology"`
	Requires map[string]RequireBinding `json:"requires,omitempty"`

	Reconcile ReconcileOptions `json:"reconcile"`

	// secrets 是密钥明文，参数名 → 值，由 ResolveSecrets / WithSecrets 附上。
	//
	// **未导出**，因此 encoding/json 根本看不见它。这不是约定而是结构上的
	// 保证：明文不可能进 digest（ComputeDigest 走 json.Marshal）、不可能
	// 进归档、不可能被某个新写的日志语句顺手打出来。
	secrets map[string]string
}

// SiteRef 标识一个 Site。
type SiteRef struct {
	Name   string            `json:"name"`
	Kind   string            `json:"kind"` // edge | cluster | standalone
	Labels map[string]string `json:"labels,omitempty"`
}

// NodeRef 标识规格投放的目标节点。
type NodeRef struct {
	Name    string            `json:"name"`
	Address string            `json:"address"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Roots 是该节点的路径根，来自 mechlet.yaml。
	Roots map[string]string `json:"roots,omitempty"`
}

// PackRef 标识规格来源的 Pack。
type PackRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision int    `json:"revision"`
	Digest   string `json:"digest,omitempty"`
}

// ParamValue 是一个已解析的参数取值。
//
// mechlet **不用参数做任何调和决策**——restartRequired / reloadRequired 的
// 效果已由 mechd 算进 Resource.Notify。这里的参数只用于三件事：
// hook 环境变量、诊断展示、状态上报。
type ParamValue struct {
	Value any    `json:"value"`
	Type  string `json:"type"`
	// Sensitive 为 true 时 Value 为空——实际值走 SecretRefs（M3 引入），
	// 由 mechlet 落到 0600 临时文件后注入 hook，用完即删。
	Sensitive bool `json:"sensitive,omitempty"`
}

// PathValue 是一条已解析的路径声明。引擎按此自动创建目录（调和阶段 ①）。
type PathValue struct {
	Name string `json:"name"`
	// Values 在 kind:single 时长度为 1。
	Values   []string `json:"values"`
	Kind     string   `json:"kind"`   // single | multi
	Layout   string   `json:"layout"` // separate | inline
	LinkInto string   `json:"linkInto,omitempty"`
	DistDir  string   `json:"distDir,omitempty"`
	Mode     string   `json:"mode"`
	Owner    string   `json:"owner,omitempty"`
	Group    string   `json:"group,omitempty"`
}

// First 返回第一个路径值；无值时返回空串。
func (p PathValue) First() string {
	if len(p.Values) == 0 {
		return ""
	}
	return p.Values[0]
}

// BlobRef 是一个载荷引用。
type BlobRef struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Filename  string `json:"filename"`
	MediaType string `json:"mediaType,omitempty"`
}

// Resource 是一条已渲染的资源声明。
type Resource struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Args 是已渲染的类型参数。
	Args json.RawMessage `json:"args"`
	// DriftPolicy: report | reconcile | ignore
	DriftPolicy string `json:"driftPolicy"`
	// Notify 是**最终动作**，已由 mechd 结合参数的 restartRequired /
	// reloadRequired 算好。mechlet 只做聚合去重与 restart 吸收 reload。
	Notify string `json:"notify,omitempty"`
	// Origin 是 "shared" 或 "role"，仅供诊断。
	Origin string `json:"origin,omitempty"`
}

// Workload 是角色的受监管进程声明。
type Workload struct {
	Runtime string           `json:"runtime"` // systemd | docker | compose
	Systemd *SystemdWorkload `json:"systemd,omitempty"`
	Docker  json.RawMessage  `json:"docker,omitempty"`
	Compose json.RawMessage  `json:"compose,omitempty"`
}

// SystemdWorkload 是 systemd runtime 的已渲染参数。
type SystemdWorkload struct {
	Exec        string            `json:"exec"`
	ExecReload  string            `json:"execReload,omitempty"`
	User        string            `json:"user,omitempty"`
	Group       string            `json:"group,omitempty"`
	WorkingDir  string            `json:"workingDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFile     string            `json:"envFile,omitempty"`
	Restart     string            `json:"restart,omitempty"`
	RestartSec  string            `json:"restartSec,omitempty"`
	LimitNofile int               `json:"limitNofile,omitempty"`
	KillMode    string            `json:"killMode,omitempty"`
	TimeoutStop string            `json:"timeoutStop,omitempty"`
	ExtraUnit   string            `json:"extraUnit,omitempty"`
}

// Health 是健康检查声明。三种探针互斥。
type Health struct {
	HTTP *HTTPProbe `json:"http,omitempty"`
	TCP  *TCPProbe  `json:"tcp,omitempty"`
	Exec *ExecProbe `json:"exec,omitempty"`

	StartupGrace     string `json:"startupGrace,omitempty"`
	Interval         string `json:"interval,omitempty"`
	Timeout          string `json:"timeout,omitempty"`
	FailureThreshold int    `json:"failureThreshold,omitempty"`
	SuccessThreshold int    `json:"successThreshold,omitempty"`
}

// HTTPProbe 是 HTTP 探针。
type HTTPProbe struct {
	Path         string `json:"path"`
	Port         int    `json:"port"`
	Scheme       string `json:"scheme,omitempty"`
	ExpectStatus []int  `json:"expectStatus,omitempty"`
}

// TCPProbe 是 TCP 探针。
type TCPProbe struct {
	Port int `json:"port"`
}

// ExecProbe 是命令探针。
type ExecProbe struct {
	Command []string `json:"command"`
}

// Hook 是一条待执行的生命周期脚本。
type Hook struct {
	Point   string   `json:"point"` // preInstall / postUpgrade / …
	Script  string   `json:"script"`
	Args    []string `json:"args,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	User    string   `json:"user,omitempty"`
}

// Topology 是放置结果的快照。
type Topology struct {
	Roles map[string][]Instance `json:"roles"`
}

// Instance 是拓扑中的一个对等实例。
type Instance struct {
	Node    string            `json:"node"`
	Address string            `json:"address"`
	Ordinal int               `json:"ordinal"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Paths 是该实例**在它自己那台节点上**解析出的路径。
	// MinIO 的端点列表需要枚举每个对等节点各自的盘路径，而节点间挂载点可以不同。
	Paths map[string][]string `json:"paths,omitempty"`
}

// RequireBinding 是一条依赖的解析结果。
type RequireBinding struct {
	Pack      string `json:"pack"`
	Component string `json:"component"`
	Version   string `json:"version"`
	Scope     string `json:"scope"` // node | site
	// Paths 仅在 scope:node 时存在——scope:site 的依赖在别的机器上，
	// 引用它的路径必然是 bug。
	Paths map[string]string `json:"paths,omitempty"`
	// Exports 是已拼装好的连接串。
	Exports map[string]string `json:"exports,omitempty"`
}

// ReconcileOptions 控制调和行为。三个周期彼此独立。
type ReconcileOptions struct {
	Interval          string `json:"interval,omitempty"`          // 默认 60s
	HealthInterval    string `json:"healthInterval,omitempty"`    // 默认 15s
	ReportInterval    string `json:"reportInterval,omitempty"`    // 默认 15s
	RetainGenerations int    `json:"retainGenerations,omitempty"` // 默认 3

	// AllowDriftRestart 允许「自动改回漂移」时顺带重启服务。
	//
	// **默认 false**：运维只是想试个参数，服务却在他手底下重启了——
	// 这是最不该由工具自作主张的一类动作。需要它时由部署方显式打开。
	AllowDriftRestart bool `json:"allowDriftRestart,omitempty"`
}

// 默认值。
const (
	DefaultInterval          = "60s"
	DefaultHealthInterval    = "15s"
	DefaultReportInterval    = "15s"
	DefaultRetainGenerations = 3
)

// WithDefaults 返回填好默认值的副本。
func (r ReconcileOptions) WithDefaults() ReconcileOptions {
	if r.Interval == "" {
		r.Interval = DefaultInterval
	}
	if r.HealthInterval == "" {
		r.HealthInterval = DefaultHealthInterval
	}
	if r.ReportInterval == "" {
		r.ReportInterval = DefaultReportInterval
	}
	if r.RetainGenerations <= 0 {
		r.RetainGenerations = DefaultRetainGenerations
	}
	return r
}

// InstanceKey 是 RoleInstance 在本机状态文件中的键，形如 "pg-main__primary"。
func (s *ResolvedSpec) InstanceKey() string {
	return s.Component + "__" + s.Role
}

// 期望运行态的取值。
const (
	// RunStateRunning 是默认：这个服务应该在跑。
	RunStateRunning = "running"
	// RunStateStopped 表示运维显式停过它，调和不该把它拉起来。
	RunStateStopped = "stopped"
	// RunStateRemoved 表示这个实例**不该存在**，收到就卸载（24-lifecycle §2.1）。
	//
	// **是「应该被删掉」而不是「已经删掉了」**——过去分词是为了和
	// running / stopped 凑成一套读着像同类的值，而这个字段本身
	// （RunState，**期望**运行态）才是消解歧义的那一半。
	//
	// 卸载不是一条指令而是一个状态值，因为指令是**事件**：丢一次就永远
	// 丢了。断连三天的节点回来之后仍然收到「这个实例不该存在」，照做即可
	// ——mechd 不需要记着「我还欠它一条卸载指令」。
	RunStateRemoved = "removed"
)

// EffectiveRunState 返回期望运行态，空串按 running 处理。
//
// **默认 running 而不是「未指定」**：一个部署出来却不知道该不该跑的组件
// 没有意义，而三态会让每个使用点都要写一遍「空串算什么」。
//
// **未知值一律按 running 处理**，这个方向是刻意的：一个旧 mechlet 收到
// 新 mechd 发来的、它不认识的运行态时，宁可让服务继续跑着（人看得见，
// 可以升级 mechlet），也不能猜成 removed 把它卸掉——那不可逆。
func (s *ResolvedSpec) EffectiveRunState() string {
	switch s.RunState {
	case RunStateStopped, RunStateRemoved:
		return s.RunState
	}
	return RunStateRunning
}

// WantsRunning 报告期望运行态是否为 running。
func (s *ResolvedSpec) WantsRunning() bool {
	return s.EffectiveRunState() == RunStateRunning
}

// WantsRemoved 报告这个实例是否应当被卸载。
func (s *ResolvedSpec) WantsRemoved() bool {
	return s.EffectiveRunState() == RunStateRemoved
}

// Removal 是卸载时的处置开关，对应 10-cli §4.3 那张「默认删什么」的表。
//
// **它随规格下发，不由 mechlet 判断**（ADR-0006）。`--purge-data` 是在
// 中心敲的，节点侧只是照做——让 mechlet 自己决定要不要删数据，等于把一个
// 不可逆的决定放在唯一没有上下文的那一端。
//
// 三个开关都是「偏离默认」的形式，因此零值就是安全的默认：
// 配置删、数据留、用户留。一份漏传 Removal 的规格不会多删任何东西。
type Removal struct {
	// KeepConfig 保留配置目录（默认删）。
	KeepConfig bool `json:"keepConfig,omitempty"`
	// PurgeData 连数据一起删（默认保留并登记为孤儿）。
	PurgeData bool `json:"purgeData,omitempty"`
	// PurgeUser 删掉 identity 资源建过的系统用户与组（默认保留）。
	//
	// 危险：那个用户可能还是别处文件的属主，删掉之后那些文件的属主
	// 变成一个裸 uid，下一个被分到同一个 uid 的用户会凭空获得它们。
	PurgeUser bool `json:"purgeUser,omitempty"`
}

// Suppression 是一条已生效的漂移抑制（06-state-and-drift §4.1）。
//
// 它随规格下发而不是由 mechlet 自己维护：`ack-drift` 发生在中心，
// 而节点侧只负责照做（ADR-0006）。到期判定也在节点侧做——比对 Until
// 与当前时刻，不需要谁去「清理」。
type Suppression struct {
	// Resource 为空表示抑制整个实例，用于整机维护窗口。
	Resource string `json:"resource,omitempty"`
	Reason   string `json:"reason"`
	// Until 是到期时刻（RFC3339）。**必须有**：没有期限的抑制会悄悄变永久，
	// 那等于把这个组件从管理中摘掉了。
	Until string `json:"until"`
}

// SuppressedAt 报告某个资源的漂移在给定时刻是否已被确认。
//
// 空 Resource 的抑制作用于整个实例——那是整机维护窗口的表达方式。
func (s *ResolvedSpec) SuppressedAt(resourceID string, now time.Time) (Suppression, bool) {
	for _, sp := range s.Suppressions {
		if sp.Resource != "" && sp.Resource != resourceID {
			continue
		}
		until, err := time.Parse(time.RFC3339, sp.Until)
		if err != nil {
			// 解析不了的期限**当作已过期**：宁可多告警一次，也不要因为
			// 一个格式错误让某处漂移永远静默
			continue
		}
		if now.Before(until) {
			return sp, true
		}
	}
	return Suppression{}, false
}
