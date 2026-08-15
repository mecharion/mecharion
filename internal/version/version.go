// Package version 提供由链接期注入的构建信息。
//
// 这些变量通过 -ldflags -X 注入，见 Makefile 的 LDFLAGS。
// 未注入时（如 go run）保留下面的开发态默认值。
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	// Version 是语义化版本号，发布时由 tag 注入。
	Version = "0.0.0-dev"
	// Commit 是构建所用的 git 提交短哈希。
	Commit = "unknown"
	// Date 是构建时间（RFC3339，UTC）。
	Date = "unknown"
)

// Info 汇总构建与运行环境信息。
type Info struct {
	Component string `json:"component" yaml:"component"`
	Version   string `json:"version"   yaml:"version"`
	Commit    string `json:"commit"    yaml:"commit"`
	Date      string `json:"date"      yaml:"date"`
	GoVersion string `json:"goVersion" yaml:"goVersion"`
	Platform  string `json:"platform"  yaml:"platform"`
}

// Get 返回指定组件的构建信息。
func Get(component string) Info {
	commit := Commit
	if commit == "unknown" {
		// go run / go build 未经 Makefile 时，从 VCS 戳中兜底取值
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					commit = s.Value[:7]
				}
			}
		}
	}
	return Info{
		Component: component,
		Version:   Version,
		Commit:    commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String 返回适合终端展示的多行文本。
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", i.Component, i.Version)
	fmt.Fprintf(&b, "  commit:    %s\n", i.Commit)
	fmt.Fprintf(&b, "  built:     %s\n", i.Date)
	fmt.Fprintf(&b, "  go:        %s\n", i.GoVersion)
	fmt.Fprintf(&b, "  platform:  %s", i.Platform)
	return b.String()
}

// Short 返回单行版本串，供日志与 User-Agent 使用。
func (i Info) Short() string {
	return fmt.Sprintf("%s/%s (%s; %s)", i.Component, i.Version, i.Commit, i.Platform)
}
