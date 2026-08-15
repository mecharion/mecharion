package pack

// Runtime 是工作负载的监管技术。
type Runtime string

const (
	// RuntimeSystemd 是裸机 + systemd（v1 主路径）。
	RuntimeSystemd Runtime = "systemd"
	// RuntimeDocker 是单容器。
	RuntimeDocker Runtime = "docker"
	// RuntimeCompose 把整个 compose project 当作一个不透明工作负载。
	RuntimeCompose Runtime = "compose"
	// RuntimePodman 预留，v1 不实现。
	RuntimePodman Runtime = "podman"
)

// KnownRuntimes 是 v1 支持的 runtime。
var KnownRuntimes = []Runtime{RuntimeSystemd, RuntimeDocker, RuntimeCompose}

// Workload 是角色的受监管进程声明。
type Workload struct {
	Runtime  Runtime          `yaml:"runtime"`
	Requires *Requires        `yaml:"requires"`
	Systemd  *SystemdWorkload `yaml:"systemd"`
	Docker   *DockerWorkload  `yaml:"docker"`
	Compose  *ComposeWorkload `yaml:"compose"`
}

// SystemdWorkload 是 systemd runtime 的参数。
type SystemdWorkload struct {
	Exec        string            `yaml:"exec"`
	ExecReload  string            `yaml:"execReload"`
	User        string            `yaml:"user"`
	Group       string            `yaml:"group"`
	WorkingDir  string            `yaml:"workingDir"`
	Env         map[string]string `yaml:"env"`
	EnvFile     string            `yaml:"envFile"`
	Restart     string            `yaml:"restart"`
	RestartSec  string            `yaml:"restartSec"`
	LimitNofile int               `yaml:"limitNofile"`
	KillMode    string            `yaml:"killMode"`
	TimeoutStop string            `yaml:"timeoutStop"`
	ExtraUnit   string            `yaml:"extraUnit"`
}

// DockerWorkload 是 docker runtime 的参数。
//
// **json 标签不可省**：这些字段会被渲染成已解析规格里的
// `workload.docker`，而那是 mechd 与 mechlet 之间的线格式。没有标签的话
// 键名会变成 Go 的字段名（`ImageBlob`），既不好看也与 spec 里印的对不上。
type DockerWorkload struct {
	ImageBlob   string            `yaml:"imageBlob"   json:"imageBlob,omitempty"`
	Command     []string          `yaml:"command"     json:"command,omitempty"`
	Args        []string          `yaml:"args"        json:"args,omitempty"`
	Env         map[string]string `yaml:"env"         json:"env,omitempty"`
	User        string            `yaml:"user"        json:"user,omitempty"`
	Mounts      []DockerMount     `yaml:"mounts"      json:"mounts,omitempty"`
	Ports       []DockerPort      `yaml:"ports"       json:"ports,omitempty"`
	Network     string            `yaml:"network"     json:"network,omitempty"`
	Restart     string            `yaml:"restart"     json:"restart,omitempty"`
	CapAdd      []string          `yaml:"capAdd"      json:"capAdd,omitempty"`
	SecurityOpt []string          `yaml:"securityOpt" json:"securityOpt,omitempty"`
	Ulimits     map[string]int    `yaml:"ulimits"     json:"ulimits,omitempty"`
}

// DockerMount 一律是 bind mount，不使用 named volume（ADR-0011）。
//
// named volume 的存储位置由 dockerd 决定，会同时打破多盘绑定设计、
// 「数据目录升级永不触碰」不变式，以及备份与现场排查的一致性。
type DockerMount struct {
	From     string `yaml:"from"     json:"from"`
	To       string `yaml:"to"       json:"to"`
	ReadOnly bool   `yaml:"readOnly" json:"readOnly,omitempty"`
}

// DockerPort 是端口映射。
type DockerPort struct {
	Host      string `yaml:"host"      json:"host"`
	Container int    `yaml:"container" json:"container"`
	Protocol  string `yaml:"protocol"  json:"protocol,omitempty"`
}

// ComposeWorkload 是 compose runtime 的参数。
//
// **File 在管道两侧含义不同**：Pack 里是 templates/ 下的模板名，已解析
// 规格里是渲染产物的绝对路径。渲染流水线负责这次改写
// （19-container-runtime §6.6.1）。
type ComposeWorkload struct {
	File        string   `yaml:"file"        json:"file,omitempty"`
	ImageBlobs  []string `yaml:"imageBlobs"  json:"imageBlobs,omitempty"`
	ProjectName string   `yaml:"projectName" json:"projectName,omitempty"`
	EnvFile     string   `yaml:"envFile"     json:"envFile,omitempty"`
	// ExecService 是 exec 探针要进的 service。project 只有一个 service
	// 时可省略；有多个而未声明时报错，不猜（ADR-0032）。
	ExecService string `yaml:"execService" json:"execService,omitempty"`
}

// ── health ──────────────────────────────────────────────────────────────

// Health 是健康检查声明。三种探针互斥。
type Health struct {
	HTTP *HTTPProbe `yaml:"http"`
	TCP  *TCPProbe  `yaml:"tcp"`
	Exec *ExecProbe `yaml:"exec"`

	StartupGrace     string `yaml:"startupGrace"`
	Interval         string `yaml:"interval"`
	Timeout          string `yaml:"timeout"`
	FailureThreshold int    `yaml:"failureThreshold"`
	SuccessThreshold int    `yaml:"successThreshold"`
}

// ProbeCount 返回已声明的探针数量，用于互斥校验。
func (h *Health) ProbeCount() int {
	n := 0
	if h.HTTP != nil {
		n++
	}
	if h.TCP != nil {
		n++
	}
	if h.Exec != nil {
		n++
	}
	return n
}

// HTTPProbe 是 HTTP 探针。
type HTTPProbe struct {
	Path         string `yaml:"path"`
	Port         string `yaml:"port"`
	Scheme       string `yaml:"scheme"`
	ExpectStatus []int  `yaml:"expectStatus"`
}

// TCPProbe 是 TCP 探针。
type TCPProbe struct {
	Port string `yaml:"port"`
}

// ExecProbe 是命令探针。
type ExecProbe struct {
	Command []string `yaml:"command"`
}
