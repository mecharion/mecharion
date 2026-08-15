package agent

import (
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/reconcile"
	"github.com/mecharion/mecharion/internal/runtime"
)

// TestLocalStatusMirrorsReport 验证的是 ADR-0026 的 `--local` 诊断
// 入口：LocalStatus 与上报给 mechd 的 report() 必须用**同一份数据、同一个
// 转换函数**（statusOf），而不是为本地视图另算一遍——否则两条路径迟早
// 在某个字段上分叉，而分叉只会在「mechd 不可达、现场靠这个排障」的
// 时刻才被发现。
func TestLocalStatusMirrorsReport(t *testing.T) {
	a := New(Options{Node: "n1"})
	a.last = map[string]*reconcile.Report{
		"pg-main/replica": {
			Component: "pg-main", Role: "replica",
			Result: reconcile.ResultOK, Generation: 7,
			Workload: &runtime.Status{
				State: runtime.StateRunning,
				Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			Health: &reconcile.HealthReport{Healthy: true},
		},
		"web/frontend": {
			Component: "web", Role: "frontend",
			Result: reconcile.ResultFailed, Generation: 3,
			Health: &reconcile.HealthReport{Healthy: false, Error: "探针超时"},
		},
	}

	got := a.LocalStatus()
	if len(got) != 2 {
		t.Fatalf("实例数 = %d，期望 2", len(got))
	}
	// LocalStatus 按 key（"<component>/<role>"）排序，"pg-main/replica" < "web/frontend"
	if got[0].Component != "pg-main" || got[0].Role != "replica" {
		t.Errorf("第一条 = %+v，期望 pg-main/replica", got[0])
	}
	if got[0].Generation != 7 {
		t.Errorf("Generation = %d，期望 7", got[0].Generation)
	}
	if got[0].Workload == nil || got[0].Workload.State != "running" {
		t.Errorf("Workload = %+v，期望 running", got[0].Workload)
	}
	if got[0].Health == nil || got[0].Health.State != "healthy" {
		t.Errorf("Health = %+v，期望 healthy", got[0].Health)
	}
	if got[1].Health == nil || got[1].Health.State != "unhealthy" || got[1].Health.LastError != "探针超时" {
		t.Errorf("第二条 Health = %+v", got[1].Health)
	}
}

// TestLocalStatusEmptyWhenNothingReconciledYet 确认没有任何实例时返回
// 空切片而不是 nil 引发的 panic 或 JSON null——调用方（mechctl --local
// component status）要能安全地 range 它、判断 len。
func TestLocalStatusEmptyWhenNothingReconciledYet(t *testing.T) {
	a := New(Options{Node: "n1"})
	got := a.LocalStatus()
	if len(got) != 0 {
		t.Fatalf("len = %d，期望 0", len(got))
	}
}
