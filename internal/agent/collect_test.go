package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/reconcile"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/state"
)

// fakeReclaimer 记下被要求删除的镜像。
type fakeReclaimer struct{ removed []string }

func (f *fakeReclaimer) RemoveImage(_ context.Context, image string) error {
	f.removed = append(f.removed, image)
	return nil
}

// stubRuntime 把 fakeReclaimer 塞进注册表。内嵌的 runtime.Runtime 是 nil：
// 回收路径只该用到 RemoveImage，真调到别的方法就会 panic。
type stubRuntime struct {
	runtime.Runtime
	*fakeReclaimer
}

func (stubRuntime) Name() string { return "fake" }

// TestCollectSkipsWhenBusy 钉住 agent 特有的那条前提：
// **有实例在忙就整轮跳过**。
//
// 那个实例可能正要 load 一个 blob，而它的台账还没写下这条引用——
// 此时回收会把正在装的东西删掉。回收可以推迟，删错不可逆。
//
// （回收本身的判据在 internal/reclaim 验收，这里只测这一条。）
func TestCollectSkipsWhenBusy(t *testing.T) {
	dir := t.TempDir()
	st, err := state.New(filepath.Join(dir, "mechlet"))
	if err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(dir, "blobs")
	sum := "cc33"
	if err := os.MkdirAll(blobDirFor(blobDir, sum), 0o755); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(blobDirFor(blobDir, sum), sum)
	if err := os.WriteFile(blobPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := st.LoadGarbage()
	if err != nil {
		t.Fatal(err)
	}
	g.Add(time.Now(), []string{"gone:1"}, []string{sum})
	if err := st.SaveGarbage(g); err != nil {
		t.Fatal(err)
	}

	rc := &fakeReclaimer{}
	a := &Agent{
		opts: Options{
			State:   st,
			BlobDir: blobDir,
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			Reconciler: &reconcile.Reconciler{
				Runtimes: runtime.NewRegistry(stubRuntime{fakeReclaimer: rc}),
			},
		},
		busy: map[string]bool{"web__default": true},
	}

	a.collectGarbage(context.Background())

	if len(rc.removed) != 0 {
		t.Errorf("有实例在忙时不该删任何镜像，实际 %v", rc.removed)
	}
	if _, err := os.Stat(blobPath); err != nil {
		t.Error("有实例在忙时不该删任何载荷")
	}

	// 不忙了就该真的删——否则这条测试也可能因为回收根本没接上而通过
	a.busy = map[string]bool{}
	a.collectGarbage(context.Background())
	if len(rc.removed) != 1 {
		t.Errorf("不忙之后应当正常回收，实际 %v", rc.removed)
	}
}
