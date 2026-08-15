package pack

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// HermeticViolation 描述一次离线约束违反。
type HermeticViolation struct {
	File    string
	Line    int
	Command string
	Reason  string
}

// hermeticRules 是拦截清单（规范 §17）。键是命令名，值是分类。
var hermeticRules = map[string]string{
	// 包管理器
	"apt": "包管理器", "apt-get": "包管理器", "aptitude": "包管理器",
	"yum": "包管理器", "dnf": "包管理器", "zypper": "包管理器",
	"apk": "包管理器", "pacman": "包管理器", "rpm": "包管理器", "dpkg": "包管理器",
	// 下载
	"curl": "下载", "wget": "下载", "scp": "下载", "rsync": "下载",
	"ftp": "下载", "sftp": "下载", "svn": "下载",
	// 语言生态
	"npm": "语言生态", "yarn": "语言生态", "pnpm": "语言生态",
	"pip": "语言生态", "pip3": "语言生态", "gem": "语言生态",
	"cargo": "语言生态", "composer": "语言生态",
	// 构建
	"make": "构建", "cmake": "构建", "mvn": "构建", "gradle": "构建",
	"ant": "构建", "gcc": "构建", "g++": "构建", "javac": "构建",
	// 容器
	"skopeo": "容器",
}

// hermeticSubcommands 是「命令本身无害、特定子命令有害」的情形。
var hermeticSubcommands = map[string]map[string]string{
	"git":     {"clone": "下载", "fetch": "下载", "pull": "下载"},
	"go":      {"get": "语言生态", "build": "构建", "install": "构建", "run": "构建"},
	"docker":  {"pull": "容器", "build": "容器"},
	"podman":  {"pull": "容器", "build": "容器"},
	"crictl":  {"pull": "容器"},
	"nerdctl": {"pull": "容器"},
}

// 匹配一行中每个「命令位置」的 token：行首、管道、&&、||、;、$( 之后。
var cmdPosRe = regexp.MustCompile(`(?:^|[|;&]|\$\(|` + "`" + `)\s*([A-Za-z0-9_./-]+)(?:\s+([A-Za-z0-9_-]+))?`)

// CheckHermetic 扫描 Pack 的 hooks/ 目录与 command/script 资源。
//
// 已知局限：静态扫描可被变量拼接绕过（`C=cur"l"; $C …`）。这是有意接受的——
// lint 的目标是防止**无意**违反，不对抗蓄意绕过。系统不做 Pack 签名/
// 来源校验（ADR-0040），蓄意绕过没有另一层机制兜底，由运维方自己判断
// 是否要部署。
func CheckHermetic(p *Pack) ([]HermeticViolation, error) {
	var out []HermeticViolation

	files, err := p.ListDir(DirHooks)
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, rel := range files {
		abs := filepath.Join(p.Dir, DirHooks, filepath.FromSlash(rel))
		vs, err := scanFile(abs, DirHooks+"/"+rel)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}

	// 内联的 command 资源
	p.AllResources(func(owner string, idx int, r Resource) {
		if r.Type != ResCommand {
			return
		}
		where := fmt.Sprintf("%s.resources[%d].command", owner, idx)
		for _, field := range []string{"run", "unless", "onlyif"} {
			if v := r.Arg(field); v != "" {
				for _, hit := range scanLine(v) {
					out = append(out, HermeticViolation{
						File: where + "." + field, Line: r.Line,
						Command: hit.cmd, Reason: hit.reason,
					})
				}
			}
		}
	})

	// 非 hermetic 的资源类型
	p.AllResources(func(owner string, idx int, r Resource) {
		if reason, bad := NonHermeticResourceTypes[r.Type]; bad {
			out = append(out, HermeticViolation{
				File:    fmt.Sprintf("%s.resources[%d]", owner, idx),
				Line:    r.Line,
				Command: r.Type,
				Reason:  reason,
			})
		}
	})

	return out, nil
}

func scanFile(abs, display string) ([]HermeticViolation, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", display, err)
	}
	defer f.Close()

	var out []HermeticViolation
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if i := indexUnquoted(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, hit := range scanLine(line) {
			out = append(out, HermeticViolation{
				File: display, Line: lineNo, Command: hit.cmd, Reason: hit.reason,
			})
		}
	}
	return out, sc.Err()
}

type hit struct{ cmd, reason string }

func scanLine(line string) []hit {
	var out []hit
	seen := map[string]bool{}

	for _, m := range cmdPosRe.FindAllStringSubmatch(line, -1) {
		raw := m[1]
		sub := m[2]
		base := filepath.Base(strings.TrimSpace(raw))

		if reason, bad := hermeticRules[base]; bad {
			key := base
			if !seen[key] {
				seen[key] = true
				out = append(out, hit{cmd: base, reason: reason})
			}
			continue
		}
		if subs, ok := hermeticSubcommands[base]; ok && sub != "" {
			if reason, bad := subs[sub]; bad {
				key := base + " " + sub
				if !seen[key] {
					seen[key] = true
					out = append(out, hit{cmd: key, reason: reason})
				}
			}
		}
	}
	return out
}

// indexUnquoted 返回首个不在引号内的字符位置；找不到返回 -1。
func indexUnquoted(s string, target byte) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == target:
			return i
		}
	}
	return -1
}

// checkHermetic 把违反项转成 lint findings（R33）。
func (l *linter) checkHermetic() {
	vs, err := CheckHermetic(l.p)
	if err != nil {
		l.err("R33", DirHooks, 0, err.Error(), "")
		return
	}
	for _, v := range vs {
		l.err("R33", v.File, v.Line,
			fmt.Sprintf("外部依赖调用: %s（%s）", v.Command, v.Reason),
			"部署阶段不允许任何外部服务依赖。依赖应在组件开发阶段解决，产物随 Pack 分发")
	}
}
