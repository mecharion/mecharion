package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// CurrentLink 是指向当前 generation 的软链名。
//
// 运行期一律引用它（spec §8.2）：Pack 在 unit 里写 {{ .Paths.Current }}，
// 因此换 generation 就是换这一条软链的指向。
const CurrentLink = "current"

// generationsDir 是 generation 目录的容器名。
const generationsDir = "generations"

// plan 是本轮调和对 generation 的决策。
type plan struct {
	// Seq 是本轮使用的 generation 序号。
	Seq int
	// Dir 是 generation 目录；组件没有 home 路径时为空。
	Dir string
	// New 表示这是一个刚分配的 generation（首次安装或内容变了）。
	New bool
	// Rollback 表示命中了一个已保留的历史 generation。
	Rollback bool
	// Switch 表示需要切换 current 软链。
	Switch bool
	// Blocked 表示这个 digest 上次失败过，本轮不自动尝试。
	//
	// 它不是「跳过一次」，是**停下来等人**：解锁靠一个新的 digest。
	Blocked bool
}

// Reused 报告本轮是否复用了当前正在跑的 generation。
func (p plan) Reused() bool { return !p.New && !p.Rollback }

// planGeneration 决定本轮用哪个 generation。
//
// 三条规则（12-spec-and-state §1.5）：
//
//	digest == 当前 generation  → 无操作，复用
//	digest 命中已保留的历史    → 回滚：目录还在，直接切软链，秒级完成
//	新 digest                  → 分配新序号，物化，切换
func planGeneration(in *state.Instance, s *spec.ResolvedSpec, home string) (plan, error) {
	if s.Digest == "" {
		return plan{}, faults.Permanentf("规划 generation",
			"规格缺少 digest —— 它是 generation 的身份，不能为空")
	}

	// 组件没有 home（纯主机配置 Pack）时不存在 generation 目录。
	// 台账仍然记，因为「这份期望状态变过没有」与有没有目录无关。
	needDir := home != ""
	if !needDir && spec.HasUnresolvedPlaceholder(s) {
		return plan{}, faults.Permanentf("规划 generation",
			"规格引用了 %s，但组件没有声明 home 路径——"+
				"generation 目录位于 <home>/%s 之下",
			spec.GenerationPlaceholder, generationsDir)
	}

	if active := in.Active(); active != nil && active.Digest == s.Digest {
		p := plan{Seq: active.Seq, Dir: active.Dir}
		// 目录被人删了就当没物化过，重新来一遍
		if needDir && !dirExists(active.Dir) {
			p.New, p.Switch = true, true
			p.Seq = in.NextSeq()
			p.Dir = generationDir(home, p.Seq, s.Pack)
		}
		return p, nil
	}

	if hit := in.FindGeneration(s.Digest); hit != nil {
		// **这个 digest 上次就失败了，而且有一个旧版本可以停留 → 不再自动切换。**
		//
		// 不锁的话，一个起不来的版本会每个周期被重试一次，而每次重试
		// 都要停一次服务——在两个坏状态之间反复横跳，比停在旧版糟得多。
		// 漂移检测不受影响：被锁住的只有「切 generation」这个动作。
		//
		// 解锁靠**新的 digest**：用户改了参数、换了版本，就是一次新的
		// 尝试。不设时限，因为「等一会儿再自己试一次」正是横跳。
		//
		// **「有旧版本可以停留」这个条件不能省。** 只判 GenFailed 的话，
		// 首次安装时一次瞬时的健康检查失败（依赖服务还没起来、端口刚好
		// 被占、或者别的操作正在停这个服务）会把这个组件**永久卡死**——
		// 而首装根本没有服务可丢，重试是完全正确的。锁要防的是
		// 「反复停机」，而首装时没有机可停。
		if hit.State == state.GenFailed && hasFallback(in, hit.Seq) {
			return plan{Seq: hit.Seq, Dir: hit.Dir, Blocked: true}, nil
		}
		if !needDir || dirExists(hit.Dir) {
			// 回滚：目录还在，不需要重新物化
			return plan{Seq: hit.Seq, Dir: hit.Dir, Rollback: true, Switch: true}, nil
		}
	}

	seq := in.NextSeq()
	p := plan{Seq: seq, New: true, Switch: true}
	if needDir {
		p.Dir = generationDir(home, seq, s.Pack)
	}
	return p, nil
}

// generationDir 拼出 generation 目录名。
//
// 形如 `<home>/generations/0007-16.4-1`：序号自增保证唯一与有序，
// 后缀带上游版本与 Pack revision 是**给现场排障的人看的**——
// `ls generations/` 就能看出装过哪些版本（04-paths-and-storage §2）。
func generationDir(home string, seq int, p spec.PackRef) string {
	name := fmt.Sprintf("%04d", seq)
	if v := sanitizeSegment(p.Version); v != "" {
		name += "-" + v
	}
	if p.Revision > 0 {
		name += "-" + fmt.Sprint(p.Revision)
	}
	return filepath.Join(home, generationsDir, name)
}

// sanitizeSegment 把版本号里不适合做目录名的字符换掉。
func sanitizeSegment(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '+':
			return r
		default:
			return '-'
		}
	}, v)
}

// makeGenerationDir 建出 generation 目录。
//
// 0755 而非更严：generation 里装的是可执行文件与只读资产，服务进程往往
// 以非 root 用户运行，进不去就起不来。真正需要保密的是配置文件，那些由
// template 资源按自己声明的 mode 落盘。
func makeGenerationDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return faults.Wrap(faults.Transient, "创建 generation 目录", err)
	}
	return nil
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// switchCurrent 原子地把 current 软链指到 dir。
//
// **这是整个升级流程里唯一不可分割的时刻**（06-state-and-drift §5）。
// 先 unlink 再 symlink 会留下一个软链不存在的窗口，systemd 在那一瞬间
// 启动服务就会失败；「建临时名 + rename」没有这个窗口。
func switchCurrent(home, dir string) error {
	if home == "" || dir == "" {
		return nil
	}
	link := filepath.Join(home, CurrentLink)

	if cur, err := os.Readlink(link); err == nil && cur == dir {
		return nil
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return faults.Wrap(faults.Transient, "创建安装根", err)
	}

	// 占位的不是软链就拒绝——那可能是用户手工放的东西，
	// 或者上一次安装留下的真实目录，删掉它是破坏性的。
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		return faults.Permanentf("切换 current",
			"%s 已存在且不是软链，无法切换 generation；请手工确认后移除", link)
	}

	tmp := filepath.Join(home, "."+CurrentLink+".tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(dir, tmp); err != nil {
		return faults.Wrap(faults.Transient, "创建 current 软链", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return faults.Wrap(faults.Transient, "切换 current 软链", err)
	}
	return nil
}

// garbage 是被回收掉的那几代所引用的镜像与载荷。
//
// **它们只是候选，不是可删清单**：同一个镜像可能还被别的实例引用，
// 那个判据是全局的，这里看不到（22-upgrade §2.5 ③）。
type garbage struct {
	images []string
	blobs  []string
}

func (g garbage) empty() bool { return len(g.images) == 0 && len(g.blobs) == 0 }

// pruneGenerations 按保留数回收旧 generation 目录。
//
// 回收的是**目录**，台账里的记录一并移除——留着一条指向已删目录的记录，
// 会让回滚在「命中 digest」之后才发现目录不在，那时已经停了服务。
//
// 第二个返回值是这几代引用过的镜像与载荷：记录一删，这些引用就再也无从
// 得知，因此必须在删之前交出去。
func pruneGenerations(in *state.Instance, retain int) ([]string, garbage) {
	var removed []string
	var junk garbage
	for _, g := range in.Prunable(retain) {
		if g.Dir != "" {
			if err := os.RemoveAll(g.Dir); err != nil {
				// 删不掉就把记录留着，下一轮再试。空间没回收是小事，
				// 台账与磁盘不一致是大事。
				continue
			}
		}
		junk.images = append(junk.images, g.Images...)
		junk.blobs = append(junk.blobs, g.Blobs...)
		in.RemoveGeneration(g.Seq)
		removed = append(removed, fmt.Sprintf("%04d", g.Seq))
	}
	return removed, junk
}

// setGenerationRefs 记下某一代引用的载荷与镜像。
//
// 覆盖而不是追加：同一代重新物化时镜像引用可能变了（Pack 换了 blob 却
// 没换 digest 是不可能的，但 `docker load` 的输出格式变了是可能的），
// 追加会让台账里堆下永远不会被删的旧引用。
func setGenerationRefs(in *state.Instance, seq int, blobs, images []string) {
	for i := range in.Generations {
		if in.Generations[i].Seq != seq {
			continue
		}
		if len(blobs) > 0 {
			in.Generations[i].Blobs = blobs
		}
		if len(images) > 0 {
			in.Generations[i].Images = dedup(images)
		}
		return
	}
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// hasFallback 报告除 seq 之外，是否还有一个可以停留的 generation。
//
// 「可以停留」= 存在、目录还在、且不是失败的那个。有它才谈得上
// 「停在旧版等人来看」；没有它就只能重试——首装失败时机器上本来
// 就没有服务，重试不会伤到任何人。
func hasFallback(in *state.Instance, seq int) bool {
	for _, g := range in.Generations {
		if g.Seq == seq || g.State == state.GenFailed {
			continue
		}
		if g.Dir == "" || dirExists(g.Dir) {
			return true
		}
	}
	return false
}
