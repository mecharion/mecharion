package mechletcmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mecharion/mecharion/internal/agent"
	"github.com/mecharion/mecharion/internal/protocol"
)

// DefaultLocalSocket 是 `--local` 诊断入口的地址（ADR-0026、10-cli §1.5）。
//
// **只读、只本机可达**：这是「mechd 不可达时，你 SSH 进现场那一刻」的
// 出路，不是常规操作入口——常规操作走 mechd，这样审计、多节点视图、
// 期望状态收敛判定才有意义。
const DefaultLocalSocket = "/run/mecharion/mechlet.sock"

// LocalStatusView 是 `GET /local/v1/status` 的响应体。
//
// Instances 直接复用 `protocol.InstanceStatus`——与 mechlet 上报给 mechd
// 的是同一个类型，不为本地视图另起一套字段。
type LocalStatusView struct {
	Node      string                    `json:"node"`
	Instances []protocol.InstanceStatus `json:"instances"`
}

// localStatusHandler 提供只读诊断视图，直接读 Agent 上报给 mechd 的
// 同一份数据（Agent.LocalStatus），不做任何写操作。
//
// **只接 GET**：这条路径存在的唯一理由是「网络断了、你 SSH 进现场那
// 一刻」，任何写操作都会在 mechd 恢复后被覆盖，写了也没有意义
// （10-cli §1.5：「降级必须只读——否则本地会写出一份与 mechd 分叉的
// 期望状态，重连后被覆盖」）。这里干脆不提供写路径，不是靠约定不写。
func localStatusHandler(a *agent.Agent, node string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(LocalStatusView{
			Node: node, Instances: a.LocalStatus(),
		})
	})
	return mux
}

// listenLocalUnix 建一个 0600 的 unix socket。
//
// 与 `mechdcmd.listenUnix` 是同一个模式（mechd serve.go）：**文件系统
// 权限就是这条通道的认证**，只有 root 能连，而 mechlet 本就以 root
// 运行——加 mTLS 是给一扇已经关好的门再挂一把锁（ADR-0026）。两处各自
// 一份而不是共用一个包，是因为这段逻辑够小（建目录、清残留、监听、
// chmod 四步），拆成公共包换来的抽象成本比维护两份四行代码更高。
func listenLocalUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}
