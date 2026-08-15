package state

import (
	"testing"
	"time"
)

func ids(items []GarbageItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func sameIDs(got []GarbageItem, want ...string) bool {
	g := ids(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// TestGarbageAddIsIdempotent 钉住「重复加不重复记，也不刷新时间」。
//
// Since 要回答「它躺在这里多久了」——每轮调和刷一次会让这个问题永远
// 得到「刚刚」这个无用的答案，而一条挂了三个月的记录本身就是诊断信息。
func TestGarbageAddIsIdempotent(t *testing.T) {
	g := &Garbage{}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.Add(t0, []string{"img:1"}, []string{"aa"})
	g.Add(t0.Add(time.Hour), []string{"img:1"}, []string{"aa", "bb"})

	if !sameIDs(g.Images, "img:1") {
		t.Errorf("镜像不该重复记，实际 %v", ids(g.Images))
	}
	if !sameIDs(g.Blobs, "aa", "bb") {
		t.Errorf("载荷应当是 aa、bb，实际 %v", ids(g.Blobs))
	}
	if !g.Images[0].Since.Equal(t0) {
		t.Errorf("重复添加不该刷新 Since，实际 %s", g.Images[0].Since)
	}
}

func TestGarbageDrop(t *testing.T) {
	g := &Garbage{}
	g.Add(time.Now(), []string{"a", "b", "c"}, []string{"x", "y"})
	g.Drop([]string{"b"}, []string{"x", "y"})

	if !sameIDs(g.Images, "a", "c") {
		t.Errorf("images = %v", ids(g.Images))
	}
	if len(g.Blobs) != 0 {
		t.Errorf("blobs 应当被清空，实际 %v", ids(g.Blobs))
	}
}

// TestLiveRefsSpansAllInstances 钉住「判据是全局的」。
//
// 一个载荷可以被多个实例、多个组件共用（内容寻址的直接后果）。
// 只看一个实例就删，会把另一个实例还在用的东西删掉。
func TestLiveRefsSpansAllInstances(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(in *Instance) {
		t.Helper()
		if err := s.SaveInstance(in); err != nil {
			t.Fatal(err)
		}
	}
	must(&Instance{Component: "web", Role: "default", Generations: []Generation{
		{Seq: 1, Images: []string{"img:1"}, Blobs: []string{"shared"}},
	}})
	must(&Instance{Component: "api", Role: "default", Generations: []Generation{
		{Seq: 1, Images: []string{"img:2"}, Blobs: []string{"shared", "only-api"}},
	}})

	images, blobs, err := s.LiveRefs()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"img:1", "img:2"} {
		if !images[want] {
			t.Errorf("%s 应当算作还在引用", want)
		}
	}
	for _, want := range []string{"shared", "only-api"} {
		if !blobs[want] {
			t.Errorf("%s 应当算作还在引用", want)
		}
	}
}

// 清单不存在时要给出空清单而不是 nil，调用方因此不必分辨「没有」与「读失败」。
func TestLoadGarbageOnFreshNode(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.LoadGarbage()
	if err != nil {
		t.Fatalf("全新节点上读回收清单不该报错: %v", err)
	}
	if g == nil {
		t.Fatal("应当返回空清单而不是 nil")
	}
	if len(g.Images) != 0 || len(g.Blobs) != 0 {
		t.Errorf("全新节点上清单应当是空的，实际 %v / %v", ids(g.Images), ids(g.Blobs))
	}
}
