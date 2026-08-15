package reclaim

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/state"
)

// fakeReclaimer 记下被要求删除的镜像。
type fakeReclaimer struct {
	removed []string
	err     error
}

func (f *fakeReclaimer) RemoveImage(_ context.Context, image string) error {
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, image)
	return nil
}

// stubRuntime 把 fakeReclaimer 塞进注册表。
//
// 内嵌的 runtime.Runtime 是 nil：回收路径只该用到 RemoveImage，
// 真调到别的方法就会 panic——那正是我们想要的断言。
type stubRuntime struct {
	runtime.Runtime
	*fakeReclaimer
}

func (stubRuntime) Name() string { return "fake" }

type fixture struct {
	t       *testing.T
	store   *state.Store
	blobDir string
	rc      *fakeReclaimer
	opts    Options
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := state.New(filepath.Join(dir, "mechlet"))
	if err != nil {
		t.Fatal(err)
	}
	rc := &fakeReclaimer{}
	f := &fixture{t: t, store: st, blobDir: filepath.Join(dir, "blobs"), rc: rc}
	f.opts = Options{
		State:    st,
		Runtimes: runtime.NewRegistry(stubRuntime{fakeReclaimer: rc}),
		BlobDir:  f.blobDir,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

func (f *fixture) writeBlob(sum string) {
	f.t.Helper()
	dir := filepath.Join(f.blobDir, "sha256", sum[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sum), []byte("x"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) blobExists(sum string) bool {
	_, err := os.Stat(filepath.Join(f.blobDir, "sha256", sum[:2], sum))
	return err == nil
}

func (f *fixture) saveInstance(component string, gens ...state.Generation) {
	f.t.Helper()
	in := &state.Instance{Component: component, Role: "default", Generations: gens}
	if err := f.store.SaveInstance(in); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) queue(images, blobs []string) {
	f.t.Helper()
	g, err := f.store.LoadGarbage()
	if err != nil {
		f.t.Fatal(err)
	}
	g.Add(time.Now(), images, blobs)
	if err := f.store.SaveGarbage(g); err != nil {
		f.t.Fatal(err)
	}
}

// TestKeepsImagesStillReferenced 是 **M6 第 7 步的验收**：
// 保留中的 generation 引用的镜像不被删。
//
// 这一条是整步的意义所在——镜像没了，那一代就不可回滚了，而
// 「可以回滚」正是保留它的全部理由（22-upgrade §2.5）。
func TestKeepsImagesStillReferenced(t *testing.T) {
	f := newFixture(t)
	f.writeBlob("aa11")
	f.writeBlob("bb22")

	// web 保留着一代，它引用 keep:1 与 blob aa11
	f.saveInstance("web", state.Generation{
		Seq: 1, State: state.GenRetained,
		Images: []string{"keep:1"}, Blobs: []string{"aa11"},
	})
	// 两者都在回收清单里（被别的实例 prune 掉时进的清单）
	f.queue([]string{"keep:1", "gone:1"}, []string{"aa11", "bb22"})

	Run(context.Background(), f.opts)

	if got := f.rc.removed; len(got) != 1 || got[0] != "gone:1" {
		t.Errorf("只该删没人引用的镜像，实际删了 %v", got)
	}
	if !f.blobExists("aa11") {
		t.Error("还被引用的载荷不该被删")
	}
	if f.blobExists("bb22") {
		t.Error("没人引用的载荷应当被删")
	}

	// 清单应当被清空：还在引用的那两条不是垃圾，也该划掉
	g, err := f.store.LoadGarbage()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Images) != 0 || len(g.Blobs) != 0 {
		t.Errorf("处理过的候选应当从清单划掉，实际 images=%v blobs=%v",
			g.Images, g.Blobs)
	}
}

// TestKeepsFailedItemsQueued 钉住「删不掉的留在清单里」。
//
// 一次失败不该让候选消失——那等于把「还没删掉」这件事忘掉，
// 而磁盘上的东西不会自己消失。
func TestKeepsFailedItemsQueued(t *testing.T) {
	f := newFixture(t)
	f.rc.err = errors.New("测试用错误")
	f.queue([]string{"gone:1"}, nil)

	Run(context.Background(), f.opts)

	g, err := f.store.LoadGarbage()
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Images) != 1 || g.Images[0].ID != "gone:1" {
		t.Errorf("删失败的候选应当留在清单里，实际 %v", g.Images)
	}
}

// TestRunsOnEmptyNode 钉住「一台被清空的机器也要回收」。
//
// 没有实例时 reconcileAll 曾经直接 return，于是最需要回收的那台机器
// 反而永远不回收。
func TestRunsOnEmptyNode(t *testing.T) {
	f := newFixture(t)
	f.writeBlob("dd44")
	f.queue([]string{"gone:1"}, []string{"dd44"})

	got := Run(context.Background(), f.opts)

	if len(got.Images) != 1 || len(got.Blobs) != 1 {
		t.Errorf("没有实例时也该回收，实际 %+v", got)
	}
	if f.blobExists("dd44") {
		t.Error("没有实例时载荷也该被回收")
	}
}

// TestNeverTouchesUnlistedImages 钉住「只删清单里的」。
//
// 镜像没有标签可依，因此判据不能是「镜像库里多出来的」——别人拉的镜像
// 从没被我们物化过，也就从没进过任何一代台账（22-upgrade §2.5 ④）。
func TestNeverTouchesUnlistedImages(t *testing.T) {
	f := newFixture(t)
	f.saveInstance("web", state.Generation{
		Seq: 1, State: state.GenActive, Images: []string{"ours:1"},
	})
	// 清单是空的：没有任何一代被 prune 过
	Run(context.Background(), f.opts)

	if len(f.rc.removed) != 0 {
		t.Errorf("清单为空时不该删任何镜像，实际 %v", f.rc.removed)
	}
}
