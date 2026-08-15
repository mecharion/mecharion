package vault

import (
	"context"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
)

// TestRenderStoreDoesNotChurnVersion 钉住「值没变就不写」。
//
// Vault.Put 每次都自增版本，而版本参与 spec digest。无条件写的后果是
// 每轮调和都产生新 generation、重渲染、重启服务——一个什么都没改的部署
// 会永远滚动下去。
func TestRenderStoreDoesNotChurnVersion(t *testing.T) {
	f := newFixture(t)
	v, cid := f.open(), f.compID
	rs := NewRenderStore(context.Background(), v, cid)

	first, err := rs.Store("demo", "pw", "same-value-every-time")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := rs.Store("demo", "pw", "same-value-every-time")
		if err != nil {
			t.Fatal(err)
		}
		if again.Version != first.Version {
			t.Fatalf("值没变时版本不该自增：第 %d 次得到 v%d，首次是 v%d",
				i+1, again.Version, first.Version)
		}
	}

	changed, err := rs.Store("demo", "pw", "a-different-value")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version == first.Version {
		t.Error("值变了必须自增版本，否则新口令发不到节点上")
	}
}

// TestRenderStoreIDIsStable 钉住引用 id 的稳定性。
func TestRenderStoreIDIsStable(t *testing.T) {
	f := newFixture(t)
	v, cid := f.open(), f.compID
	rs := NewRenderStore(context.Background(), v, cid)

	a, err := rs.Ensure("demo", "pw", pack.Generate{Length: 16})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rs.Ensure("demo", "pw", pack.Generate{Length: 16})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("引用 id 必须稳定，实际 %q 与 %q", a.ID, b.ID)
	}
	if a.Value != b.Value {
		t.Error("generate 只在首次——第二次必须返回同一个值")
	}
}
