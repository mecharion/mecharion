package reconcile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// removed 把一份规格改成「这个实例不该存在」。
func removed(opts *spec.Removal) func(*spec.ResolvedSpec) {
	return func(s *spec.ResolvedSpec) {
		s.RunState = spec.RunStateRemoved
		s.Removal = opts
	}
}

// installed 先正常装一遍，返回三个目录。
func (f *fixture) installed() (home, config, data string) {
	f.t.Helper()
	f.MustReconcile(f.webappSpec())
	return f.path("opt", "mecharion", "apps", "webapp"),
		f.path("etc", "mecharion", "apps", "webapp"),
		f.path("var", "lib", "mecharion", "apps", "webapp")
}

// ── 默认处置 ────────────────────────────────────────────────────────────

// TestRemovedTearsDownWorkloadAndGeneration 是这一步的主判据。
func TestRemovedTearsDownWorkloadAndGeneration(t *testing.T) {
	f := newFixture(t)
	home, config, data := f.installed()
	f.RT.Reset()

	rep := f.MustReconcile(f.webappSpec(removed(nil)))

	if rep.Removed == nil {
		t.Fatal("卸载之后 Report.Removed 必须非 nil——mechd 靠它判定「拆完了」")
	}
	if exists(home) {
		t.Errorf("home 目录（含 generations）应当被删掉: %s", home)
	}
	if exists(config) {
		t.Errorf("配置目录默认删: %s", config)
	}
	// **默认保留数据**，与「升级永不触碰数据目录」同源
	if !exists(data) {
		t.Errorf("数据目录默认保留，不该被删: %s", data)
	}

	// 停 → 卸，顺序不能反
	acts := strings.Join(f.RT.Actions(), ",")
	if !strings.Contains(acts, "stop") || !strings.Contains(acts, "remove") {
		t.Errorf("应当先 stop 再 remove，实际动作: %v", f.RT.Actions())
	}
	if strings.Index(acts, "stop") > strings.Index(acts, "remove") {
		t.Errorf("stop 必须排在 remove 之前，实际: %v", f.RT.Actions())
	}
}

// TestRemovedNeverMaterializes 守的是一条会被「反正结果对了」掩盖掉的错误。
//
// Ref 此前只有 Materialize 产得出来。卸载路径若走它，机器上会先写一遍
// unit 文件、`docker load` 一个几百 MB 的镜像，然后立刻删掉——而这在
// 最终状态上**完全看不出来**：目录照样没了，测试照样绿。
//
// 代价要到真机上才现形：一次卸载多花几分钟，且中途失败会把「删除」
// 变成「装了一半」。因此判据只能是「过程里有没有 materialize」。
func TestRemovedNeverMaterializes(t *testing.T) {
	f := newFixture(t)
	f.installed()
	f.RT.Reset()

	f.MustReconcile(f.webappSpec(removed(nil)))

	for _, a := range f.RT.Actions() {
		if a == "materialize" {
			t.Fatalf("卸载路径物化了一遍：拆之前不能先装。动作: %v", f.RT.Actions())
		}
	}
	// 反面：RefFor 必须被调过，否则上面那条断言是空的
	var sawRefFor bool
	for _, a := range f.RT.Actions() {
		if a == "refFor" {
			sawRefFor = true
		}
	}
	if !sawRefFor {
		t.Errorf("没走 RefFor，那上面「没物化」的断言什么也没证明: %v", f.RT.Actions())
	}
}

// ── 三个开关 ────────────────────────────────────────────────────────────

func TestPurgeDataAlsoDeletesData(t *testing.T) {
	f := newFixture(t)
	_, _, data := f.installed()

	rep := f.MustReconcile(f.webappSpec(removed(&spec.Removal{PurgeData: true})))

	if exists(data) {
		t.Errorf("给了 --purge-data，数据目录应当被删: %s", data)
	}
	if len(rep.Removed.RetainedPaths) != 0 {
		t.Errorf("什么都不该保留，实际 %v", rep.Removed.RetainedPaths)
	}
}

func TestKeepConfigKeepsConfig(t *testing.T) {
	f := newFixture(t)
	_, config, data := f.installed()

	rep := f.MustReconcile(f.webappSpec(removed(&spec.Removal{KeepConfig: true})))

	if !exists(config) {
		t.Errorf("给了 --keep-config，配置目录应当保留: %s", config)
	}
	if !contains(rep.Removed.RetainedPaths, config) {
		t.Errorf("保留下来的目录必须登记，否则没人找得到它。实际 %v",
			rep.Removed.RetainedPaths)
	}
	if !contains(rep.Removed.RetainedPaths, data) {
		t.Errorf("数据目录也该在保留清单里，实际 %v", rep.Removed.RetainedPaths)
	}
}

// TestCustomPathNamesAreRetained 是「未归类的一律保留」这条方向选择的判据。
//
// HDFS 用的是 dataDirs 这种自定义名。若归类只认预定义名而把其余当作
// 可删，一次 remove 会**在默认参数下删掉多盘数据**——这个项目里最不可
// 接受的一类事故。
func TestCustomPathNamesAreRetained(t *testing.T) {
	f := newFixture(t)
	disk1 := f.path("data1", "dfs")
	disk2 := f.path("data2", "dfs")
	withDisks := func(s *spec.ResolvedSpec) {
		s.Paths["dataDirs"] = spec.PathValue{
			Name: "dataDirs", Values: []string{disk1, disk2},
			Kind: "multi", Mode: "0750",
		}
	}
	f.MustReconcile(f.webappSpec(withDisks))

	rep := f.MustReconcile(f.webappSpec(withDisks, removed(nil)))

	for _, d := range []string{disk1, disk2} {
		if !exists(d) {
			t.Errorf("自定义名的多盘目录默认必须保留: %s", d)
		}
		if !contains(rep.Removed.RetainedPaths, d) {
			t.Errorf("%s 没登记进保留清单: %v", d, rep.Removed.RetainedPaths)
		}
	}
}

// ── 本地状态 ────────────────────────────────────────────────────────────

// TestCleanRemovalLeavesNoState：什么都没留时状态文件整个删掉。
func TestCleanRemovalLeavesNoState(t *testing.T) {
	f := newFixture(t)
	f.installed()

	f.MustReconcile(f.webappSpec(removed(&spec.Removal{PurgeData: true})))

	if in := f.Instance("webapp", "default"); in != nil {
		t.Errorf("一次干净的卸载不该留下任何痕迹，实际还有状态: %+v", in)
	}
}

// TestRetainedRemovalLeavesAReceipt 守的是 orphans 的地基。
//
// 卸完就把状态文件删掉，保留下来的数据目录就再也没人知道来历——
// 而「保留而不提供发现机制等于把问题推给未来」（10-cli §4.3）。
func TestRetainedRemovalLeavesAReceipt(t *testing.T) {
	f := newFixture(t)
	_, _, data := f.installed()

	f.MustReconcile(f.webappSpec(removed(nil)))

	in := f.Instance("webapp", "default")
	if in == nil || in.Removed == nil {
		t.Fatal("留了数据目录就必须留一张收据，否则 orphans 列不出它")
	}
	if !contains(in.Removed.RetainedPaths, data) {
		t.Errorf("收据里没有数据目录: %+v", in.Removed)
	}
	// 来历：一堆没有出处的数据回答不了「能不能删」
	if in.Removed.Pack != "go-webapp" || in.Removed.Version != "1.2.0" {
		t.Errorf("收据要记下是谁、哪一版留下的，实际 pack=%q version=%q",
			in.Removed.Pack, in.Removed.Version)
	}
	// 收据不是一个「装着的实例」
	if len(in.Generations) != 0 || in.CurrentGeneration != 0 {
		t.Errorf("台账必须清空，否则回收与 orphans 都会误判: %+v", in.Generations)
	}
}

// TestRemovalReleasesBlobAndImageClaims 钉住 remove 的一条副作用：
// **blob 与镜像的引用计数 −1**（10-cli §4.3 那张表的最后一行）。
//
// 收据的 Generations 是空的，因此它不再声称引用任何载荷——那些载荷于是
// 变得可回收。少了这一条，一次 remove 之后组件的载荷会永远占着盘，
// 而没有任何地方会报错。
//
// 判据取自 Store.LiveRefs（回收的唯一输入），而不是直接看收据的字段：
// 后者只能证明「我把它清空了」，前者才能证明「回收真的看得见这件事」。
func TestRemovalReleasesBlobAndImageClaims(t *testing.T) {
	f := newFixture(t)
	f.installed()

	// 前提：装着的时候确实认领着东西，否则下面的断言什么也证明不了
	images, _, err := f.Store.LiveRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 {
		t.Fatal("装着的时候就没有任何认领，这条测试无从证伪")
	}

	f.MustReconcile(f.webappSpec(removed(nil)))

	after, _, err := f.Store.LiveRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("卸载之后不该再认领任何载荷，否则它永远回收不掉: %v", after)
	}
}

// ── 幂等 ────────────────────────────────────────────────────────────────

// TestRemovedIsIdempotent 是 removing 能不能收尾的前提。
//
// removed 是一个会被**反复下发**的状态：mechd 要等全部实例都报「已卸载」
// 才删记录，在那之前每一轮调和都会再走一遍这条路。任何一处
// 「已经没了 → 报错」都会让组件永远卡在 removing。
func TestRemovedIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.installed()

	for i := 1; i <= 3; i++ {
		rep, err := f.Reconcile(f.webappSpec(removed(nil)))
		if err != nil {
			t.Fatalf("第 %d 次卸载失败: %v\n%s", i, err, rep.Summary())
		}
		if rep.Removed == nil {
			t.Fatalf("第 %d 次没报 Removed——mechd 会一直等下去", i)
		}
	}
}

// TestRemovedOnNeverInstalledSucceeds：从没装过也要报成功。
//
// 一台在部署失败之后加进来的节点、或一个 --force 之后重新上线的实例，
// 都会走到这里。报错的话，那个组件同样卡死。
func TestRemovedOnNeverInstalledSucceeds(t *testing.T) {
	f := newFixture(t)

	rep, err := f.Reconcile(f.webappSpec(removed(nil)))
	if err != nil {
		t.Fatalf("从没装过的实例收到 removed 应当直接成功: %v", err)
	}
	if rep.Removed == nil {
		t.Fatal("仍然要报 Removed")
	}
	// **不能凭空登记孤儿。** 从没装过 = 那些目录压根没被建出来，
	// 把它们写进保留清单，orphans 里就会出现一条指向空地址的记录——
	// 而运维会真的跑去那台机器上找它。
	if len(rep.Removed.RetainedPaths) != 0 {
		t.Errorf("目录根本不存在，不该登记为「保留」: %v", rep.Removed.RetainedPaths)
	}
	if in := f.Instance("webapp", "default"); in != nil {
		t.Errorf("更不该留下一张收据: %+v", in.Removed)
	}
}

// TestRemovedSkipsStopHooksWhenAlreadyStopped 守的是重复卸载时的 hook 噪声。
//
// preStop 的语义是「服务马上要停了」。对一个早就停着的东西每轮喊一遍，
// 会让 hook 作者写的「摘流量」之类动作在反复的卸载轮次里重复执行。
func TestRemovedSkipsStopHooksWhenAlreadyStopped(t *testing.T) {
	f := newFixture(t)
	f.installed()
	f.MustReconcile(f.webappSpec(removed(nil))) // 第一次：真的停了
	f.RT.Reset()

	f.MustReconcile(f.webappSpec(removed(nil))) // 第二次：已经 absent

	for _, a := range f.RT.Actions() {
		if a == "stop" {
			t.Errorf("已经停掉的东西不该再 stop 一次: %v", f.RT.Actions())
		}
	}
}

// ── 路径漂移 ────────────────────────────────────────────────────────────

// TestRemovedUsesPinnedPathsNotSpecPaths 守的是「删不掉的实例」这个死角。
//
// 正常路径上 CheckPaths 会拒绝「规格里的路径与固化值不一致」的调和。
// 卸载若也守这条，一个路径漂移过的实例就再也删不掉了——而它恰恰是最
// 需要被删掉的那种。拆的必须是**盘上真实存在的那一份**。
func TestRemovedUsesPinnedPathsNotSpecPaths(t *testing.T) {
	f := newFixture(t)
	_, _, realData := f.installed()

	moved := func(s *spec.ResolvedSpec) {
		p := s.Paths["data"]
		p.Values = []string{f.path("srv", "elsewhere")}
		s.Paths["data"] = p
	}
	rep, err := f.Reconcile(f.webappSpec(moved, removed(nil)))
	if err != nil {
		t.Fatalf("路径漂移过的实例也必须删得掉: %v", err)
	}
	if !contains(rep.Removed.RetainedPaths, realData) {
		t.Errorf("保留的应当是固化的那个路径 %s，实际 %v",
			realData, rep.Removed.RetainedPaths)
	}
}

// ── --purge-user ────────────────────────────────────────────────────────

func withUser(s *spec.ResolvedSpec) {
	s.Resources = append(s.Resources,
		spec.Resource{ID: "group:webapp", Type: "group",
			Args: mustJSON(map[string]any{"name": "webapp", "system": true})},
		spec.Resource{ID: "user:webapp", Type: "user",
			Args: mustJSON(map[string]any{"name": "webapp", "system": true, "group": "webapp"})},
	)
}

func TestPurgeUserDeletesUserThenGroup(t *testing.T) {
	f := newFixture(t)
	f.MustReconcile(f.webappSpec(withUser))

	rep := f.MustReconcile(f.webappSpec(withUser, removed(&spec.Removal{PurgeUser: true})))

	var order []string
	for _, c := range f.Runner.Calls() {
		if verb, _, _ := strings.Cut(c, " "); verb == "userdel" || verb == "groupdel" {
			order = append(order, c)
		}
	}
	want := []string{"userdel webapp", "groupdel webapp"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		// 顺序反了 groupdel 会因为组还被用户引用而失败
		t.Errorf("应当先 userdel 再 groupdel，实际 %v", order)
	}
	if !contains(rep.Removed.PurgedIdentities, "webapp") {
		t.Errorf("删掉的身份要登记，实际 %v", rep.Removed.PurgedIdentities)
	}
}

func TestWithoutPurgeUserNothingIsDeleted(t *testing.T) {
	f := newFixture(t)
	f.MustReconcile(f.webappSpec(withUser))

	f.MustReconcile(f.webappSpec(withUser, removed(nil)))

	for _, c := range f.Runner.Calls() {
		if verb, _, _ := strings.Cut(c, " "); verb == "userdel" || verb == "groupdel" {
			t.Errorf("没给 --purge-user 时一个身份都不该删，实际跑了 %s", c)
		}
	}
}

// TestPurgeUserFailureDoesNotFailRemoval 是一条方向性判据。
//
// userdel 在用户还有进程、或还是别处文件属主时会拒绝——那是常态。
// 让它中止卸载，组件就会永远卡在 removing：为一个可选的清理动作
// 赌上整条删除路径，不划算。但也不能静默：警告要回到敲命令的人面前。
func TestPurgeUserFailureDoesNotFailRemoval(t *testing.T) {
	f := newFixture(t)
	f.MustReconcile(f.webappSpec(withUser))
	f.Runner.SetPrefix("userdel ", command.Result{
		ExitCode: 8, Stderr: "userdel: user webapp is currently used by process 1234",
	})

	rep, err := f.Reconcile(f.webappSpec(withUser, removed(&spec.Removal{PurgeUser: true})))
	if err != nil {
		t.Fatalf("userdel 失败不该让卸载失败: %v", err)
	}
	if len(rep.Removed.Warnings) == 0 {
		t.Fatal("也不能静默——warning 必须回到敲 remove 的那个人面前")
	}
	if !strings.Contains(strings.Join(rep.Removed.Warnings, "\n"), "webapp") {
		t.Errorf("warning 要说清是哪个身份没删掉: %v", rep.Removed.Warnings)
	}
}

// ── 归类表 ──────────────────────────────────────────────────────────────

func TestDispositionTable(t *testing.T) {
	for name, want := range map[string]spec.Disposition{
		"home":     spec.DropAlways,
		"runtime":  spec.DropAlways,
		"config":   spec.DropUnlessKept,
		"data":     spec.KeepUnlessPurged,
		"logs":     spec.KeepUnlessPurged,
		"dataDirs": spec.KeepUnlessPurged, // 自定义名
		"cache":    spec.KeepUnlessPurged, // 未归类的一律保留
	} {
		if got := spec.DispositionOf(name); got != want {
			t.Errorf("DispositionOf(%q) = %v，期望 %v", name, got, want)
		}
	}
}

// TestPathsUnderHomeAreNotRetained 守的是「指向空地址的孤儿」。
//
// layout: inline 的配置物理位于 generation 内，也就是 home 之下。删 home
// 已经把它带走了；若还把它登记为「保留」，orphans 会列出一个不存在的
// 目录——那比漏登记更糟，因为它把人指向一个空地址。
func TestPathsUnderHomeAreNotRetained(t *testing.T) {
	f := newFixture(t)
	home := f.path("opt", "mecharion", "apps", "webapp")
	inner := filepath.Join(home, "scratch")
	withInner := func(s *spec.ResolvedSpec) {
		s.Paths["scratch"] = spec.PathValue{
			Name: "scratch", Values: []string{inner}, Kind: "single", Mode: "0755",
		}
	}
	f.MustReconcile(f.webappSpec(withInner))

	rep := f.MustReconcile(f.webappSpec(withInner, removed(nil)))

	if contains(rep.Removed.RetainedPaths, inner) {
		t.Errorf("home 之下的目录已随 home 消失，不该登记为保留: %v",
			rep.Removed.RetainedPaths)
	}
	if exists(inner) {
		t.Errorf("它应当已经随 home 一起没了: %s", inner)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestNearMissPathNamesAreRetained 把「未归类一律保留」的代价钉成明文。
//
// 归类只认**精确的**预定义名。一个把配置目录起名叫 `conf`（而不是
// `config`）的 Pack，它的配置目录会被当成数据保留下来——mechd 的
// paramkit 夹具正是这样写的。
//
// **这是刻意的，不是缺陷。** 模糊匹配（conf ≈ config ≈ etc）会让归类
// 变得不可预测，而这里唯一不能接受的是「猜错了方向去删东西」。留错
// 可以 orphans purge，删错回不来。
//
// 代价照实说：Pack 作者若想让配置目录随卸载一起消失，**必须把它命名为
// `config`**。这条约束目前只写在文档里，没有任何工具会提醒他。
func TestNearMissPathNamesAreRetained(t *testing.T) {
	for _, name := range []string{"conf", "etc", "configs", "Config"} {
		if got := spec.DispositionOf(name); got != spec.KeepUnlessPurged {
			t.Errorf("DispositionOf(%q) = %v，期望保留——归类只认精确的预定义名",
				name, got)
		}
	}
	// 精确的那个才是删
	if spec.DispositionOf("config") != spec.DropUnlessKept {
		t.Error("`config` 这个精确名字必须落在「默认删」上")
	}
}
