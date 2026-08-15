//go:build !linux

package mechletcmd

import "runtime"

// osFamily 在非 Linux 上退回 GOOS。
//
// mechlet 的生产形态只有 Linux；这些实现存在只是为了让 `go build` 在
// 开发机上过得去（平台隔离编译，见 00-overview）。
func osFamily() string { return runtime.GOOS }

// memTotal 在非 Linux 上不采集。
//
// 返回 0 而不是猜一个值：`defaultFrom` 求值失败会回落到 default 并告警，
// 那比拿一个编造的数字算出配置要好。
func memTotal() int64 { return 0 }
