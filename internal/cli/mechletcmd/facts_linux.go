//go:build linux

package mechletcmd

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func osFamily() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "linux"
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id, ok := strings.CutPrefix(sc.Text(), "ID="); ok {
			return strings.Trim(id, `"`)
		}
	}
	return "linux"
}

// memTotal 返回内存总量的字节数。
//
// 读 /proc/meminfo 而非 sysinfo(2)：前者不需要 cgo，也不需要平台相关的
// syscall 结构体，而这个值只用于 defaultFrom 的估算，精度足够。
func memTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
