package ctlcmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// secret 参数的三种输入（10-cli §4.3）。
//
// `--set` 用于 secret 类型参数时**直接报错**：命令行会同时进入 shell
// history 与同机任何用户都看得到的 `ps aux`。
//
// **这条规则只能在客户端执行。** 服务端分辨不出一个值是 `--set` 还是
// `--set-file` 传来的——两者到它手上是同一个 map。而风险本来就全在客户端：
// 用户敲下那行命令的瞬间，明文已经进了 history，服务端再拒也追不回来。
//
// 因此客户端在**发出请求之前**先问一次参数声明，就地拒绝。多一次 GET，
// 换的是「这条纪律真的成立」——而在此之前它只是文档里的一句话。

// setStdinUsage 是 `--set-stdin` 的说明。
const setStdinUsage = "Read a parameter value from stdin (pipe or interactive TTY)"

// paramTypes 向 mechd 问一份参数名 → 类型。
//
// component 非空时问已部署组件（那份带着当前取值与来源），否则问 Pack
// 的声明——`deploy` 一个新组件时它还不存在。
func paramTypes(f *ClientFlags, pack, component, profile string) (map[string]string, error) {
	cli, err := f.client()
	if err != nil {
		return nil, validationErr(err)
	}
	path := mechd.APIPrefix + "/packs/" + seg(pack) + "/params" + query(map[string]string{"profile": profile})
	if component != "" {
		path = configPath(component, "")
	}

	var out formResp
	if err := cli.Do("GET", path, nil, &out); err != nil {
		return nil, err
	}
	types := map[string]string{}
	for _, p := range out.Params {
		if p.Sensitive {
			// sensitive 与 secret 都不该走命令行：前者只是「不回显」，
			// 但它同样会进 history 与 ps
			types[p.Name] = "secret"
			continue
		}
		types[p.Name] = p.Type
	}
	return types, nil
}

// rejectPlaintextSecrets 拦下用 `--set` 传的 secret 参数。
//
// 拿不到类型时**放行并警告**，不阻断：mechd 不可达、Pack 名打错、
// 老版本服务端——这些都会让类型查不到，而把它们变成「部署失败」是把一条
// 卫生规则升级成了可用性问题。警告让人知道这次没检查。
func rejectPlaintextSecrets(
	c *cobra.Command, f *ClientFlags, pack, component, profile string, sets []string,
) error {
	if len(sets) == 0 {
		return nil
	}
	types, err := paramTypes(f, pack, component, profile)
	if err != nil {
		fmt.Fprintf(c.ErrOrStderr(),
			"note: couldn't fetch parameter declarations, --set wasn't checked for secrets this time (%v)\n", err)
		return nil
	}

	var bad []string
	for _, s := range sets {
		k, _, ok := strings.Cut(s, "=")
		if !ok {
			continue // 格式问题由 parseSets 报
		}
		if types[k] == "secret" || strings.HasPrefix(types[k], "list<secret") {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)

	var b strings.Builder
	for _, name := range bad {
		fmt.Fprintf(&b, "parameter %s is a secret, can't be passed as plaintext via --set\n", name)
	}
	b.WriteString("  It would end up in both shell history and ps aux. Use instead:\n")
	for _, name := range bad {
		fmt.Fprintf(&b, "    --set-file %s=@/run/secrets/%s   preferred for unattended use\n", name, name)
		fmt.Fprintf(&b, "    --set-stdin %s                   pipe or interactive\n", name)
	}
	return validationErr(fmt.Errorf("%s", strings.TrimRight(b.String(), "\n")))
}

// readStdinSecrets 处理 `--set-stdin`。
//
// 两种来源，行为不同：
//
//	管道   一次读完全部，按参数顺序切行——无人值守的路
//	TTY    逐个提示、不回显
//
// **非 TTY 且没有输入时报错，不静默用空值**：一个自动化脚本里少接了
// 管道，静默用空口令会让组件带着空密码起来，而那要到很久以后才被发现。
func readStdinSecrets(c *cobra.Command, names []string) (map[string]any, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := map[string]any{}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		for _, name := range names {
			fmt.Fprintf(c.ErrOrStderr(), "%s: ", name)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(c.ErrOrStderr())
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", name, err)
			}
			if len(b) == 0 {
				return nil, validationErr(fmt.Errorf("%s cannot be empty", name))
			}
			out[name] = string(b)
		}
		return out, nil
	}

	// 非 TTY：从管道读。多个参数时一行一个，顺序与 --set-stdin 的给出顺序一致。
	data, err := io.ReadAll(c.InOrStdin())
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, validationErr(fmt.Errorf(
			"--set-stdin needs a value read from stdin, but stdin is empty\n"+
				"  pipe: vault read -field=pw … | mechctl … --set-stdin %s\n"+
				"  file: --set-file %s=@/run/secrets/%s",
			names[0], names[0], names[0]))
	}
	if len(lines) < len(names) {
		return nil, validationErr(fmt.Errorf(
			"got %d --set-stdin parameter(s), but stdin only has %d line(s) — one value per line",
			len(names), len(lines)))
	}
	for i, name := range names {
		out[name] = strings.TrimRight(lines[i], "\r")
	}
	return out, nil
}
