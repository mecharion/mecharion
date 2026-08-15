package store

import (
	"errors"
	"testing"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/ident"
)

// TestRepoRejectsInvalidIdentifiers 验证：四类实体的写入口是全仓库
// 唯一能落库它们的地方，任何一处放过非法名字都会被这条测试抓到。
//
// 只测「拦住了没有」，不重复 internal/ident 已经覆盖过的字符集穷举——
// 那部分的责任在 ident 包自己的表驱动测试里。
func TestRepoRejectsInvalidIdentifiers(t *testing.T) {
	f := newRepoFixture(t)
	bad := "../evil"

	t.Run("Site", func(t *testing.T) {
		_, err := f.r.Sites().Create(bg(), Site{Name: bad, Kind: "standalone"})
		assertRejected(t, err)
	})

	t.Run("Node", func(t *testing.T) {
		_, err := f.r.Nodes().Upsert(bg(), Node{SiteID: f.site.ID, Name: bad})
		assertRejected(t, err)
	})

	t.Run("Component", func(t *testing.T) {
		_, err := f.r.Components().Create(bg(), Component{
			SiteID: f.site.ID, Name: bad,
			Pack: PackRef{Name: "zookeeper", Version: "3.9.1", Revision: 1},
		})
		assertRejected(t, err)
	})

	t.Run("ConfigGroup", func(t *testing.T) {
		c := f.component("zk")
		_, err := f.r.ConfigGroups().Upsert(bg(), ConfigGroup{
			ComponentID: c.ID, Role: "server", Name: bad,
		})
		assertRejected(t, err)
	})
}

func assertRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("非法名字应当被拒绝，实际写入成功")
	}
	if !errors.Is(err, ident.ErrInvalid) {
		t.Errorf("错误 %v 未包裹 ident.ErrInvalid", err)
	}
	if faults.ClassOf(err) != faults.Permanent {
		t.Errorf("错误分类 = %v，期望 Permanent（否则 HTTP 层会当成 500）", faults.ClassOf(err))
	}
}
