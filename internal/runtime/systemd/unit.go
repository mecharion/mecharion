package systemd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// UnitName 返回一个角色实例的 unit 名。
//
// 形如 `mecharion-pg-main-primary.service`。用短前缀而非反向 DNS，
// 是 systemd 生态的惯例（09-naming-conventions §systemd unit 前缀）。
func UnitName(component, role string) string {
	return "mecharion-" + component + "-" + role + ".service"
}

// renderUnit 生成 unit 文件内容。
func renderUnit(w runtime.WorkloadSpec) (string, error) {
	sd := w.Workload.Systemd
	if sd == nil {
		return "", faults.Permanentf("生成 unit", "workload.runtime=systemd 但缺少 systemd 段")
	}
	if err := validateExec(sd.Exec); err != nil {
		return "", err
	}
	if sd.ExecReload != "" {
		if err := validateExec(sd.ExecReload); err != nil {
			return "", err
		}
	}

	var b strings.Builder

	// 头部注释是给现场排障的人看的：这个 unit 从哪来、属于哪个
	// generation、不要手工改（原则七 现场可诊断）。
	b.WriteString("# 由 Mecharion 生成，请勿手工编辑。\n")
	b.WriteString("# 修改会在下一次调和（默认 60 秒）时被覆盖。\n")
	fmt.Fprintf(&b, "# component=%s role=%s", w.Component, w.Role)
	if w.ConfigGroup != "" {
		fmt.Fprintf(&b, " configGroup=%s", w.ConfigGroup)
	}
	if w.Generation > 0 {
		fmt.Fprintf(&b, " generation=%04d", w.Generation)
	}
	b.WriteString("\n\n")

	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=Mecharion %s/%s\n", w.Component, w.Role)
	b.WriteString("Documentation=https://mecharion.dev\n")
	// network-online 而非 network：绑定具体地址的服务在 network.target
	// 时地址还没配好，会以「Cannot assign requested address」启动失败。
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", sd.Exec)
	if sd.ExecReload != "" {
		fmt.Fprintf(&b, "ExecReload=%s\n", sd.ExecReload)
	}
	writeIf(&b, "User", sd.User)
	writeIf(&b, "Group", sd.Group)
	writeIf(&b, "WorkingDirectory", sd.WorkingDir)

	for _, k := range sortedKeys(sd.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", quoteEnv(k, sd.Env[k]))
	}
	if sd.EnvFile != "" {
		// 前缀 `-` 表示文件不存在时不算错误。EnvFile 常由 template 资源
		// 生成，而 unit 可能在它之前就被 systemd 读到。
		fmt.Fprintf(&b, "EnvironmentFile=-%s\n", sd.EnvFile)
	}

	restart := sd.Restart
	if restart == "" {
		restart = "on-failure"
	}
	fmt.Fprintf(&b, "Restart=%s\n", restart)
	writeIf(&b, "RestartSec", sd.RestartSec)
	if sd.LimitNofile > 0 {
		fmt.Fprintf(&b, "LimitNOFILE=%d\n", sd.LimitNofile)
	}
	writeIf(&b, "KillMode", sd.KillMode)
	writeIf(&b, "TimeoutStopSec", sd.TimeoutStop)

	if extra := strings.TrimRight(sd.ExtraUnit, "\n"); extra != "" {
		b.WriteString(extra)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String(), nil
}

func writeIf(b *strings.Builder, key, val string) {
	if val != "" {
		fmt.Fprintf(b, "%s=%s\n", key, val)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 排序让 unit 内容对同一份规格稳定——否则 map 遍历顺序会让每次
	// 调和都「检测到 unit 变了」，进而无谓地 daemon-reload。
	sort.Strings(out)
	return out
}

// quoteEnv 生成一条 `Environment=` 的取值。
//
// systemd 按 shell 风格拆分这一行，因此含空格的值必须加引号，
// 而值里的 `"` 与 `\` 必须转义。漏掉这步会让一个带空格的 JAVA_OPTS
// 被拆成多个环境变量——症状是「参数莫名其妙丢了一半」。
func quoteEnv(key, val string) string {
	if val == "" {
		return key + `=""`
	}
	if !strings.ContainsAny(val, " \t\n\"'\\$") {
		return key + "=" + val
	}
	var b strings.Builder
	b.WriteString(key)
	b.WriteString(`="`)
	for _, r := range val {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// validateExec 检查命令行的第一个词是绝对路径。
//
// systemd 不经过 shell，ExecStart 的第一个词必须是可执行文件的绝对路径。
// 写成相对路径时 systemd 要到 start 的那一刻才报错，而且报的是
// 「Failed at step EXEC」——完全看不出是路径问题。在这里拦掉。
func validateExec(cmdline string) error {
	// 占位符检查必须在绝对路径检查之前：`{{ .Paths.Generation }}/bin/x`
	// 也不是绝对路径，但报「请先 ResolveGeneration」比报「要绝对路径」
	// 精确得多，而后者会把人引向完全错误的方向。
	if strings.Contains(cmdline, spec.GenerationPlaceholder) {
		return faults.Permanentf("生成 unit",
			"exec 中残留了未替换的 %s——物化前必须先调用 spec.ResolveGeneration",
			spec.GenerationPlaceholder)
	}
	first, err := firstWord(cmdline)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(first, "/") {
		return faults.Permanentf("生成 unit",
			"exec 的第一个词必须是绝对路径，实际 %q\n"+
				"  systemd 不经过 shell，不会在 PATH 中查找命令", first)
	}
	return nil
}

// firstWord 取出命令行的第一个词，认得引号。
func firstWord(cmdline string) (string, error) {
	s := strings.TrimLeft(cmdline, " \t")
	if s == "" {
		return "", faults.Permanentf("生成 unit", "exec 为空")
	}
	// systemd 允许 ExecStart 以 `-` `@` `+` `!` 等前缀修饰
	s = strings.TrimLeft(s, "-@+!:")
	if s == "" {
		return "", faults.Permanentf("生成 unit", "exec 只有修饰前缀，没有命令")
	}
	if s[0] == '"' || s[0] == '\'' {
		q := s[0]
		if i := strings.IndexByte(s[1:], q); i >= 0 {
			return s[1 : 1+i], nil
		}
		return "", faults.Permanentf("生成 unit", "exec 中的引号没有闭合: %s", cmdline)
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], nil
	}
	return s, nil
}
