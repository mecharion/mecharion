package mechd

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/spec"
)

// TestSetRunStateRefusesRemoved 守的是一条绕开全部安全网的捷径。
//
// `removed` 与 `running` / `stopped` 是同一个字段的三个值，因此
// `SetRunState` 天然就「差一点」能设它。但卸载那条路上有引用检查、
// 影响面打印与二档确认（10-cli §4.3），而这个动词一个都没有——
// 放行它，`component stop --state removed` 就成了一条静默卸载命令。
func TestSetRunStateRefusesRemoved(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	_, err := f.svc.SetRunState(ctx(), SetRunStateRequest{
		Component: "paramkit", State: spec.RunStateRemoved, Actor: "test",
	})
	if err == nil {
		t.Fatal("SetRunState 必须拒绝 removed——那会绕开二档确认与引用检查")
	}
	// 拒绝还不够，要把人引到对的那条路上
	if !strings.Contains(err.Error(), "component remove") {
		t.Errorf("错误里要给出正确的动词，得到: %v", err)
	}
}

// TestSetRunStateStillAcceptsRunningAndStopped 让上面那条不能靠
// 「把所有值都拒了」蒙混过关。
func TestSetRunStateStillAcceptsRunningAndStopped(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	for _, st := range []string{spec.RunStateStopped, spec.RunStateRunning} {
		n, err := f.svc.SetRunState(ctx(), SetRunStateRequest{
			Component: "paramkit", State: st, Actor: "test",
		})
		if err != nil {
			t.Fatalf("%s 应当照常放行: %v", st, err)
		}
		if n == 0 {
			t.Errorf("%s 一个实例都没改到，这条断言等于没做", st)
		}
	}
}
