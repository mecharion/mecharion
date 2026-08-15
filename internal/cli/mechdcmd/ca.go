package mechdcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/pki"
)

// NewCACmd 构造 `mechd ca`。
//
// 两条命令服务两个真实场景：
//
//	export  远程 mechctl / 浏览器要信任这套自签 CA（08-security §3.2）
//	issue   **离线**给一个节点签证书——机器连不上控制面、或者运维想
//	        把证书随镜像预置进去时，token 那条路走不通
//
// 它们与 M7 第 3 步的 Join RPC 用**同一个** pki.IssueNode。两套签发逻辑
// 意味着两套边界条件，而证书出错时症状都长得一样（一句 TLS 错误）。
func NewCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Manage the self-signed CA and node certificates",
	}
	cmd.AddCommand(newCAExportCmd(), newCAIssueCmd())
	return cmd
}

func newCAExportCmd() *cobra.Command {
	var confDir, out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the CA certificate for remote mechctl and browsers to trust",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			src := filepath.Join(pki.Dir(confDir), "ca.crt")
			body, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("read CA (has mechd never been started?): %w", err)
			}
			if out == "" {
				_, err = c.OutOrStdout().Write(body)
				return err
			}
			if err := os.WriteFile(out, body, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Exported %s\n", out)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&confDir, "conf-dir", DefaultConfDir, "Config and key directory")
	f.StringVarP(&out, "out", "", "", "Output file, defaults to stdout")
	return cmd
}

func newCAIssueCmd() *cobra.Command {
	var confDir, node, outDir string
	var validity time.Duration
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Issue a client certificate for a node",
		Long: `Issue a node certificate offline.

**The CN is the node name** — in a multi-node setup that's the only
trusted source of identity (ADR-0034). The three files you get need to be
placed under that machine's <conf-dir>/pki, named node.crt / node.key /
ca.crt.

The normal path is a join token (mechctl node token create), which doesn't
need anyone to move files around by hand. This command is for when the
machine **can't reach the control plane**, or the certificate needs to be
**baked into an image**.`,
		Example: "  mechd ca issue --node store-042 --out-dir /tmp/store-042",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if node == "" {
				return fmt.Errorf("--node cannot be empty — it is the certificate's identity")
			}
			p, err := pki.IssueNode(pki.Dir(confDir), node, validity)
			if err != nil {
				return err
			}
			if outDir == "" {
				fmt.Fprintf(c.OutOrStdout(),
					"Issued a certificate for %s\n  %s\n  %s\n", node, p.CertFile, p.KeyFile)
				return nil
			}
			if err := os.MkdirAll(outDir, 0o700); err != nil {
				return err
			}
			// 落到 out-dir 时**直接用目标机器上的文件名**：让运维整目录
			// 拷过去即可，不必再对着文档改名——改错一个名字的症状是
			// 「证书明明在那儿却读不到」。
			for _, f := range []struct{ src, dst string }{
				{p.CertFile, "node.crt"},
				{p.KeyFile, "node.key"},
				{p.CAFile, "ca.crt"},
			} {
				body, err := os.ReadFile(f.src)
				if err != nil {
					return err
				}
				mode := os.FileMode(0o644)
				if f.dst == "node.key" {
					mode = 0o600
				}
				if err := os.WriteFile(filepath.Join(outDir, f.dst), body, mode); err != nil {
					return err
				}
			}
			fmt.Fprintf(c.OutOrStdout(),
				"Issued a certificate for %s into %s\n  Copy the whole directory to that machine's <conf-dir>/pki\n",
				node, outDir)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&confDir, "conf-dir", DefaultConfDir, "Config and key directory")
	f.StringVar(&node, "node", "", "Node name (written into the certificate CN)")
	f.StringVar(&outDir, "out-dir", "", "Copy node.crt / node.key / ca.crt into this directory")
	f.DurationVar(&validity, "validity", 0, "Certificate validity period, defaults to 1 year")
	return cmd
}
