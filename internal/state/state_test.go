package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "mechlet"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNodeRoundTrip(t *testing.T) {
	s := newStore(t)

	if n, err := s.LoadNode(); err != nil || n != nil {
		t.Fatalf("不存在时应返回 (nil, nil)，实际 (%v, %v)", n, err)
	}

	want := &Node{
		NodeName: "node-7", NodeID: "abc123",
		DataDir:     "/var/lib/mecharion",
		Roots:       map[string]string{"opt": "/opt/mecharion"},
		Volumes:     []Volume{{Name: "data1", Path: "/data1", Class: "bulk"}},
		Upstream:    "unix:///run/mecharion/mechd.sock",
		InstalledAt: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	}
	if err := s.SaveNode(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeName != want.NodeName || got.DataDir != want.DataDir ||
		got.Roots["opt"] != "/opt/mecharion" || len(got.Volumes) != 1 {
		t.Errorf("往返后内容不符: %+v", got)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion 应被自动填充，实际 %d", got.SchemaVersion)
	}
}

// TestCheckDataDir 钉住「data-dir 固化后不可变」。
func TestCheckDataDir(t *testing.T) {
	n := &Node{DataDir: "/var/lib/mecharion"}

	if err := n.CheckDataDir("/var/lib/mecharion"); err != nil {
		t.Errorf("一致时不应报错: %v", err)
	}
	err := n.CheckDataDir("/data/mecharion")
	if err == nil {
		t.Fatal("不一致时必须拒绝——迁移数据目录是运维动作，工具不该替用户决定")
	}
	for _, want := range []string{"已固化", "/var/lib/mecharion", "/data/mecharion"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际: %v", want, err)
		}
	}

	// 首次安装（尚未固化）不应报错
	if err := (&Node{}).CheckDataDir("/anything"); err != nil {
		t.Errorf("未固化时不应报错: %v", err)
	}
}

func TestInstanceRoundTrip(t *testing.T) {
	s := newStore(t)
	key := InstanceKey("pg-main", "primary")

	if in, err := s.LoadInstance(key); err != nil || in != nil {
		t.Fatalf("不存在时应返回 (nil, nil)")
	}

	in := &Instance{
		Component: "pg-main", Role: "primary", ConfigGroup: "default",
		Paths: map[string][]string{
			"data": {"/var/lib/mecharion/apps/pg-main"},
		},
		CurrentGeneration: 7,
		AppliedResources: []ResourceRef{
			{ID: "template:/etc/postgresql.conf", Type: "template"},
		},
	}
	in.AddGeneration(Generation{Seq: 7, Digest: "aaa", State: GenActive})
	if err := s.SaveInstance(in); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadInstance(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentGeneration != 7 || len(got.Generations) != 1 ||
		len(got.AppliedResources) != 1 {
		t.Errorf("往返后内容不符: %+v", got)
	}

	keys, err := s.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != key {
		t.Errorf("ListInstances = %v", keys)
	}

	if err := s.DeleteInstance(key); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.LoadInstance(key); in != nil {
		t.Error("删除后仍能读到")
	}
}

// TestCheckPaths 钉住「路径首次物化后固化、不可变」。
func TestCheckPaths(t *testing.T) {
	in := &Instance{
		Component: "pg-main", Role: "primary",
		Paths: map[string][]string{
			"data":   {"/var/lib/mecharion/apps/pg-main"},
			"dnDirs": {"/data1/dfs", "/data2/dfs"},
		},
	}

	t.Run("一致", func(t *testing.T) {
		if err := in.CheckPaths(map[string][]string{
			"data":   {"/var/lib/mecharion/apps/pg-main"},
			"dnDirs": {"/data1/dfs", "/data2/dfs"},
		}); err != nil {
			t.Errorf("一致时不应报错: %v", err)
		}
	})

	t.Run("路径变了", func(t *testing.T) {
		err := in.CheckPaths(map[string][]string{
			"data":   {"/data1/apps/pg-main"},
			"dnDirs": {"/data1/dfs", "/data2/dfs"},
		})
		if err == nil {
			t.Fatal("路径变化必须被拒绝——否则已装组件会静默搬家，旧数据变成孤儿")
		}
		if !strings.Contains(err.Error(), "已固化") {
			t.Errorf("错误信息应说明已固化值: %v", err)
		}
	})

	t.Run("多盘顺序变了", func(t *testing.T) {
		err := in.CheckPaths(map[string][]string{
			"data":   {"/var/lib/mecharion/apps/pg-main"},
			"dnDirs": {"/data2/dfs", "/data1/dfs"},
		})
		if err == nil {
			t.Error("多盘顺序变化也应被拒绝——HDFS 等组件依赖盘顺序稳定")
		}
	})

	t.Run("路径消失", func(t *testing.T) {
		if err := in.CheckPaths(map[string][]string{
			"data": {"/var/lib/mecharion/apps/pg-main"},
		}); err == nil {
			t.Error("已固化的路径在新规格中消失，应当被拒绝")
		}
	})

	t.Run("首次物化", func(t *testing.T) {
		if err := (&Instance{}).CheckPaths(map[string][]string{"data": {"/x"}}); err != nil {
			t.Errorf("尚未固化时不应报错: %v", err)
		}
	})
}

// TestFindGeneration 钉住「回滚靠 digest 命中已保留目录，秒级完成」。
func TestFindGeneration(t *testing.T) {
	in := &Instance{}
	in.AddGeneration(Generation{Seq: 6, Digest: "old", State: GenRetained})
	in.AddGeneration(Generation{Seq: 7, Digest: "new", State: GenActive})
	in.CurrentGeneration = 7

	if g := in.FindGeneration("old"); g == nil || g.Seq != 6 {
		t.Error("应当能按 digest 命中历史 generation")
	}
	if g := in.FindGeneration("从未见过"); g != nil {
		t.Error("未知 digest 不应命中")
	}
	if a := in.Active(); a == nil || a.Seq != 7 {
		t.Error("Active 应返回 currentGeneration 对应的记录")
	}
}

func TestGenerationsSortedDescending(t *testing.T) {
	in := &Instance{}
	for _, seq := range []int{3, 1, 5, 2} {
		in.AddGeneration(Generation{Seq: seq})
	}
	want := []int{5, 3, 2, 1}
	for i, g := range in.Generations {
		if g.Seq != want[i] {
			t.Fatalf("generations 应按 seq 倒序，实际 %v", in.Generations)
		}
	}
	if got := in.NextSeq(); got != 6 {
		t.Errorf("NextSeq = %d, 期望 6", got)
	}
}

func TestSetActive(t *testing.T) {
	in := &Instance{}
	in.AddGeneration(Generation{Seq: 6, State: GenRetained})
	in.AddGeneration(Generation{Seq: 7, State: GenActive})
	in.CurrentGeneration = 7

	in.SetActive(6) // 回滚

	if in.CurrentGeneration != 6 {
		t.Errorf("CurrentGeneration = %d", in.CurrentGeneration)
	}
	for _, g := range in.Generations {
		switch g.Seq {
		case 6:
			if g.State != GenActive {
				t.Error("6 应变为 active")
			}
		case 7:
			if g.State != GenRetained {
				t.Error("原 active 应降级为 retained")
			}
		}
	}
}

// TestPrunable 钉住「active 永不回收、failed 留一个供诊断」。
func TestPrunable(t *testing.T) {
	in := &Instance{}
	in.AddGeneration(Generation{Seq: 9, State: GenActive})
	in.AddGeneration(Generation{Seq: 8, State: GenRetained})
	in.AddGeneration(Generation{Seq: 7, State: GenRetained})
	in.AddGeneration(Generation{Seq: 6, State: GenRetained})
	in.AddGeneration(Generation{Seq: 5, State: GenFailed})
	in.AddGeneration(Generation{Seq: 4, State: GenFailed})
	in.CurrentGeneration = 9

	prunable := in.Prunable(3)

	seqs := map[int]bool{}
	for _, g := range prunable {
		seqs[g.Seq] = true
	}
	if seqs[9] {
		t.Error("active 永远不能被回收")
	}
	if seqs[8] || seqs[7] {
		t.Error("保留数 3 意味着 active + 最近 2 个 retained 都要留着（回滚的落脚点）")
	}
	if !seqs[6] {
		t.Error("超出保留数的 retained 应当被回收")
	}
	if seqs[5] {
		t.Error("最近一个 failed 应保留供诊断")
	}
	if !seqs[4] {
		t.Error("更早的 failed 应当被回收")
	}
}

func TestRemoveGeneration(t *testing.T) {
	in := &Instance{}
	in.AddGeneration(Generation{Seq: 1})
	in.AddGeneration(Generation{Seq: 2})
	in.RemoveGeneration(1)
	if len(in.Generations) != 1 || in.Generations[0].Seq != 2 {
		t.Errorf("移除后 = %v", in.Generations)
	}
}

// TestAtomicWriteLeavesNoPartialFile 钉住原子写不留临时文件。
func TestAtomicWriteLeavesNoPartialFile(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		if err := s.SaveNode(&Node{NodeName: "n", DataDir: "/x"}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("残留了临时文件: %s", e.Name())
		}
	}
}

func TestWriteJSONMode(t *testing.T) {
	s := newStore(t)
	if err := s.SaveNode(&Node{NodeName: "n", DataDir: "/x"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.Root(), fileNode))
	if err != nil {
		t.Fatal(err)
	}
	// Windows 不实现 Unix 权限位，跳过
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不实现 Unix 权限位")
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Errorf("状态文件不应对属主之外可读，实际 %o", st.Mode().Perm())
	}
}
