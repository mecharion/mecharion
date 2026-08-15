// Package runtime 定义 Runtime 抽象——「进程怎么被监管」这条接缝。
//
// 设计见 docs/design/05-runtime.md 与 ADR-0010。接缝划在这里而非别处，
// 是因为一个 Pack 描述的两类东西里，只有「什么东西应该在跑」随监管技术
// 变化；「什么东西应该存在」（文件、用户、渲染好的配置）完全无关。
//
// 三条划界规则（做错了抽象就会制造重复而非消除重复）：
//
//   - **健康检查的编排不进接口，但「在哪里执行」进**。重试、阈值、超时、
//     结果归一化在任何 Runtime 下一致，写一次即可；只有 exec 探针要问
//     「在哪跑」——`pg_isready` 容器化之后只存在于镜像里。为此有 ExecIn。
//     判据：**一件事在不同 Runtime 下答案不同，它才属于接口**（ADR-0032）。
//   - **升级与回滚不进接口**。generation 切换的编排是共享的；Runtime 只
//     提供 Materialize / Stop / Start 三个原语。
//   - **Observe 必须返回归一化状态**，否则漂移检测与 UI 就得认识每种 Runtime。
package runtime

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// Runtime 监管一个角色实例的进程。
type Runtime interface {
	// Name 返回 Runtime 名，如 "systemd"。
	Name() string

	// Probe 回答「这台机器支持我吗、版本多少」。结果作为 Node capability 上报。
	Probe(ctx context.Context) (Capability, error)

	// Materialize 把工作负载所需的一切落到节点上，**但不启动**。
	//
	//	systemd: 写 unit 文件 + daemon-reload + enable
	//	docker:  docker load 离线镜像 tar + 创建容器（不 start）
	//	compose: 渲染 compose.yaml + 加载全部镜像
	Materialize(ctx context.Context, w WorkloadSpec) (Ref, error)

	// RefFor 构造 Ref 而**不碰机器**——卸载路径唯一的入口。
	//
	// 存在的理由只有一个：拆之前不能先装一遍。Ref 此前只有 Materialize
	// 产得出来，而卸载时走它意味着写一遍 unit 文件、`docker load` 一个
	// 几百 MB 的镜像，然后立刻把它们删掉。那不只是浪费——中间任何一步
	// 失败都会让「删除」变成「装了一半」。
	//
	// 因此这个方法必须是纯函数：只从 WorkloadSpec 推名字（unit 名 /
	// 容器名 / compose project 名），不 exec、不写盘、不返回 Images
	// （镜像引用只有物化那一刻知道）。
	RefFor(w WorkloadSpec) (Ref, error)

	Start(ctx context.Context, ref Ref) error
	Stop(ctx context.Context, ref Ref, opts StopOpts) error

	// Reload 触发热加载。未声明 execReload 的工作负载应返回 ErrReloadUnsupported，
	// 由调用方降级为 restart。
	Reload(ctx context.Context, ref Ref) error

	// Observe 返回归一化的运行状态——漂移检测与 UI 的唯一输入。
	Observe(ctx context.Context, ref Ref) (Status, error)

	Logs(ctx context.Context, ref Ref, opts LogOpts) (io.ReadCloser, error)

	// Remove 卸除工作负载。不删除数据与配置——那是资源引擎与 purge 的事。
	Remove(ctx context.Context, ref Ref) error

	// ExecIn 在**工作负载的上下文里**执行一条命令。
	//
	//	systemd: 就在宿主机上——工作负载的上下文就是这台机器
	//	docker:  docker exec 进容器
	//	compose: docker compose exec <service>
	//
	// 它服务两个用途：`exec` 健康探针，与 `mechctl component exec`。
	// 一个动词两个用途，而不是为健康检查特开一条路（ADR-0032）。
	//
	// **返回值的两种失败必须分开**：
	//
	//	res.ExitCode != 0  命令跑了，但失败了      → 探针失败，计入 failureThreshold
	//	err != nil         压根没跑起来（容器没在跑）→ 探不了，不该计入
	//
	// 混为一谈会让一个刚被停掉的容器看起来像「健康检查连续失败」。
	ExecIn(ctx context.Context, ref Ref, cmd []string) (command.Result, error)
}

// ── 输入 ────────────────────────────────────────────────────────────────

// WorkloadSpec 是 Materialize 的输入。
//
// 它只带 Runtime 真正需要的东西，不是整份 ResolvedSpec——把整份规格
// 传进来会让每个 Runtime 实现都能碰到它不该碰的字段。
type WorkloadSpec struct {
	Site        string
	Component   string
	Role        string
	ConfigGroup string

	// Generation 是 mechlet 本地分配的序号，用于诊断（写进 unit 注释）。
	Generation int
	// GenerationDir 是本次物化的 generation 目录。
	GenerationDir string
	// Home 是组件的安装根，`current` 软链就在它下面。
	Home string

	// SpecDigest 是本次期望状态的身份。
	//
	// systemd 用不到它（generation 目录已经代表了身份），但**容器不可变**：
	// 判断「现有容器是不是我要的那个」只能靠比对一个摘要——逐项比 env /
	// mount / port 既繁琐又必然漏，docker 会规范化一部分值，比对不出原样。
	SpecDigest string

	// Blobs 是本次可用的载荷：名字 → 本机文件路径。
	//
	// systemd 的载荷由资源引擎解压，Runtime 碰不到；docker 的镜像却要
	// Runtime 自己 `docker load`。这是两类 Runtime 在输入上唯一的实质差异。
	Blobs map[string]string

	// Workload 是已渲染的工作负载声明。
	Workload *spec.Workload
}

// BlobPath 取一个载荷在本机的路径。
func (w WorkloadSpec) BlobPath(name string) (string, bool) {
	p, ok := w.Blobs[name]
	return p, ok
}

// Key 是该工作负载在本机的稳定标识，形如 "pg-main-primary"。
func (w WorkloadSpec) Key() string { return w.Component + "-" + w.Role }

// Validate 做 Runtime 无关的基本校验。
func (w WorkloadSpec) Validate() error {
	switch {
	case w.Component == "":
		return fmt.Errorf("缺少 component")
	case w.Role == "":
		return fmt.Errorf("缺少 role")
	case w.Workload == nil:
		return fmt.Errorf("缺少 workload")
	case w.Workload.Runtime == "":
		return fmt.Errorf("缺少 workload.runtime")
	}
	return nil
}

// Ref 是一次 Materialize 的产物标识。
type Ref struct {
	// Runtime 是产出它的 Runtime 名。
	Runtime   string
	Component string
	Role      string
	// Generation 是物化时的 generation 序号，仅供诊断。
	Generation int

	// Native 是 Runtime 原生的标识：systemd unit 名 / 容器 ID。
	//
	// **不可省略**：出问题时运维需要知道去 `journalctl -u xxx` 还是
	// `docker logs yyy`（原则七 现场可诊断）。
	Native string

	// Images 是这次物化装进本地镜像库的镜像引用，供回收使用。
	//
	// **只有 Materialize 那一刻知道**：镜像引用是 `docker load` 的输出，
	// 不是规格的函数，事后无从推算（22-upgrade §2.5 ①）。不涉及镜像的
	// Runtime（systemd）留空。
	Images []string
}

func (r Ref) String() string {
	if r.Native == "" {
		return r.Component + "/" + r.Role
	}
	return r.Native
}

// ImageReclaimer 由「镜像是一等公民」的 Runtime 可选实现。
//
// **刻意不进 Runtime 接口**：systemd 没有镜像这个概念，让它实现一个空方法
// 只是为了满足接口，会让「这个 Runtime 到底管不管镜像」这个问题从类型上
// 消失。可选接口把这件事说清楚：断言得到就管，断言不到就不管。
type ImageReclaimer interface {
	// RemoveImage 从本地镜像库删除一个镜像。
	//
	// 镜像不存在**不算错误**：回收要可以重复执行，而「已经没了」正是
	// 我们想要的终态。
	RemoveImage(ctx context.Context, image string) error
}

// StopOpts 控制停止行为。
type StopOpts struct {
	// Timeout 覆盖工作负载自己声明的停止超时。零值表示按声明来。
	Timeout time.Duration
	// Force 为 true 时，超时后强杀而不是报错返回。
	Force bool
}

// LogOpts 控制取日志的范围。
type LogOpts struct {
	Follow bool
	// Tail 是从末尾取多少行；0 表示不限制。
	Tail  int
	Since time.Time
}

// ── 输出 ────────────────────────────────────────────────────────────────

// Capability 是 Probe 的结果。
type Capability struct {
	// Available 表示本机能用这个 Runtime。
	Available bool
	Version   string
	// Reason 在 Available 为 false 时说明为什么——mechd 放置失败时
	// 要把这句话原样给用户看。
	Reason string
	// Detail 是补充信息，随 Node capability 一起上报。
	Detail map[string]string
}

// State 是归一化的运行状态。
type State int

const (
	// StateAbsent 表示工作负载没有被物化：unit 未安装、容器不存在。
	StateAbsent State = iota
	// StateStopped 表示已物化但没在跑。
	StateStopped
	// StateStarting 表示正在启动。
	StateStarting
	// StateRunning 表示正常运行。
	StateRunning
	// StateFailed 表示异常退出或启动失败。
	StateFailed
	// StateDegraded 表示在跑，但处在预期之外的子状态——不好断言是好是坏，
	// 交给人看。Raw 里带着原始信息。
	StateDegraded
)

// MarshalJSON 让状态在 JSON 里是名字而不是数字。
//
// State 的底层类型是 int，不管的话 `-o json` 会输出 `"state": 3`——
// 读的人得先找到这个枚举才知道那是 running。同一个根源还咬过一次：
// `string(s)` 会得到码点对应的控制字符而不是名字。
func (s State) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON 认名字，也认旧的数字形态。
func (s *State) UnmarshalJSON(b []byte) error {
	text := strings.Trim(string(b), `"`)
	for _, cand := range []State{
		StateAbsent, StateStopped, StateStarting,
		StateRunning, StateFailed, StateDegraded,
	} {
		if cand.String() == text {
			*s = cand
			return nil
		}
	}
	// 兼容此前落盘的数字形态，免得一次升级读不了旧的报告
	n, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("未知的工作负载状态 %q", text)
	}
	*s = State(n)
	return nil
}

func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	case StateDegraded:
		return "degraded"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// HealthState 是 Runtime **原生**的健康信息（docker HEALTHCHECK、
// systemd watchdog）。
//
// 它不是 Pack 声明的 http/tcp/exec 探针——那些在 Runtime 之上执行，
// 跨 Runtime 行为一致。绝大多数 systemd 工作负载这里是 HealthNone。
type HealthState int

const (
	// HealthNone 表示该 Runtime 对这个工作负载没有原生健康信息。
	HealthNone HealthState = iota
	// HealthPassing 表示原生健康检查通过。
	HealthPassing
	// HealthFailing 表示原生健康检查失败。
	HealthFailing
)

func (h HealthState) String() string {
	switch h {
	case HealthPassing:
		return "passing"
	case HealthFailing:
		return "failing"
	default:
		return "none"
	}
}

// Status 是归一化的观测结果。
type Status struct {
	State State
	// Since 是进入当前状态的时刻。
	Since    time.Time
	Restarts int
	// ExitCode 仅在 StateFailed 时有意义。
	ExitCode *int
	Health   HealthState

	// Native 是 Runtime 原生标识，排障时给人看。
	Native string
	// Raw 是原始信息，UI 可展开。
	Raw map[string]string
}

// Running 报告工作负载是否处在正常运行状态。
func (s Status) Running() bool { return s.State == StateRunning }
