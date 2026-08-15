package ctlcmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/protocol"
)

// localStatusResponse 镜像 `mechletcmd.LocalStatusView`——两边故意不共用
// 一个类型（ctlcmd 不该依赖 mechletcmd，那是另一个二进制的命令树），
// 但字段必须逐一对上，因为解的是同一份 JSON。
type localStatusResponse struct {
	Node      string                    `json:"node"`
	Instances []protocol.InstanceStatus `json:"instances"`
}

// runLocalStatus 是 `mechctl --local component status` 的实现。
//
// **只读、只看本机**：mechlet 不知道 Site 里其它节点在跑什么，也没有
// 期望状态可比对（那份数据在 mechd），因此这里给不出「收敛了吗」，
// 只能给出「这台机器现在跑着什么、健不健康」（ADR-0026、10-cli §1.5）。
func runLocalStatus(c *cobra.Command, f *ClientFlags) error {
	cli, err := f.client()
	if err != nil {
		return validationErr(err)
	}
	var resp localStatusResponse
	if err := cli.Do("GET", "/local/v1/status", nil, &resp); err != nil {
		return err
	}

	w := c.OutOrStdout()
	switch f.output() {
	case rootcli.OutputJSON:
		return writeJSON(w, resp)
	case rootcli.OutputYAML:
		return writeYAML(w, resp)
	}

	if len(resp.Instances) == 0 {
		fmt.Fprintf(w, "No instances on %s\n", resp.Node)
		return nil
	}

	fmt.Fprintf(w, "Local mechlet read-only view (showing only instances on %s, "+
		"no desired state to compare against, can't tell if it's converged)\n\n", resp.Node)
	fmt.Fprintf(w, "%-16s %-10s %-10s %s\n", "COMPONENT", "ROLE", "GENERATION", "STATE")
	for _, in := range resp.Instances {
		fmt.Fprintf(w, "%-16s %-10s %-10d %s\n",
			in.Component, in.Role, in.Generation, localWorkloadText(in))
	}
	return nil
}

// localWorkloadText 把工作负载与健康状态合成一句话。
//
// 用 sinceText 而不是文档草图里示意的 `2d3h` 缩写形式——本仓库其余
// 所有相对时间的显示（如 `component status` 的漂移时间）都走这一个
// 帮助函数，两套时长格式并存只会让读的人多一次心算。
func localWorkloadText(in protocol.InstanceStatus) string {
	state := "unknown"
	since := ""
	if in.Workload != nil {
		state = capitalize(in.Workload.State)
		if in.Workload.Since != "" {
			since = fmt.Sprintf(" (%s)", sinceText(in.Workload.Since))
		}
	}
	text := state + since
	if in.Health != nil && in.Health.State == "unhealthy" {
		text += ", unhealthy"
	}
	return text
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
