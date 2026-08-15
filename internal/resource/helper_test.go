package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// mk 构造一条已解析的资源声明。
func mk(t *testing.T, id, typ string, args any) spec.Resource {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Resource{ID: id, Type: typ, Args: b}
}

// build 构造资源，失败即终止。
func build(t *testing.T, env *Env, r spec.Resource) Resource {
	t.Helper()
	res, err := New(env, r)
	if err != nil {
		t.Fatalf("构造 %s 资源失败: %v", r.Type, err)
	}
	return res
}

// testEnv 是一个不依赖真实外部命令的环境。
func testEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{
		PackRoot: t.TempDir(),
		BlobDir:  t.TempDir(),
		Blobs:    map[string]spec.BlobRef{},
		Runner:   newFakeRunner(),
	}
}

// putBlob 把内容写进 blob 存储，并登记到 env。
func putBlob(t *testing.T, env *Env, name string, content []byte) spec.BlobRef {
	t.Helper()
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	dir := filepath.Join(env.BlobDir, "sha256", hexSum[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hexSum), content, 0o644); err != nil {
		t.Fatal(err)
	}
	ref := spec.BlobRef{
		Name: name, SHA256: hexSum, Size: int64(len(content)),
		Filename: name + ".bin",
	}
	env.Blobs[name] = ref
	return ref
}

// blobRef 构造一条载荷声明，但**不**把内容放进 blob 存储——
// 用于测试「载荷尚未就位」。
func blobRef(name, sum string) spec.BlobRef {
	return spec.BlobRef{Name: name, SHA256: sum, Filename: name + ".bin"}
}

// ── 幂等契约 ────────────────────────────────────────────────────────────

// snapshot 是一棵目录树的可观测状态：相对路径 → 内容摘要 + 权限 + 软链目标。
//
// **不含 mtime**——不是因为 mtime 不重要，而是它由各类型的测试单独断言：
// file 要求 mtime 不变，archive 允许重解（内容一致即可）。
type snapshot map[string]string

func snap(t *testing.T, root string) snapshot {
	t.Helper()
	out := snapshot{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link→" + tgt
		case d.IsDir():
			out[rel] = "dir " + modeOf(fi)
		default:
			sum, _, err := hashFile(p)
			if err != nil {
				return err
			}
			out[rel] = "file " + modeOf(fi) + " " + sum[:16]
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// modeOf 在不实现 Unix 权限位的平台上返回占位符，让快照仍可比较。
func modeOf(fi fs.FileInfo) string {
	if runtime.GOOS == "windows" {
		return "-"
	}
	return fi.Mode().Perm().String()
}

func (s snapshot) diff(other snapshot) []string {
	var out []string
	for k, v := range s {
		if o, ok := other[k]; !ok {
			out = append(out, "消失: "+k)
		} else if o != v {
			out = append(out, k+": "+v+" → "+o)
		}
	}
	for k := range other {
		if _, ok := s[k]; !ok {
			out = append(out, "新增: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// requireIdempotent 执行 11-resource-engine.md §6 强制要求的幂等用例：
// 连续 Apply 两次，第二次不得改变 root 下的任何可观测状态。
//
// 这条是重中之重：Apply 的幂等是**实现者的责任**，接口签名不强制它。
// 一个非幂等的资源类型会在 60 秒一次的调和下反复产生副作用，而这类 bug
// 在单次 apply 的测试里发现不了。
func requireIdempotent(t *testing.T, r Resource, root string) {
	t.Helper()
	ctx := context.Background()

	if err := r.Apply(ctx); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	before := snap(t, root)

	if err := r.Apply(ctx); err != nil {
		t.Fatalf("第二次 Apply: %v", err)
	}
	if d := before.diff(snap(t, root)); len(d) > 0 {
		t.Errorf("第二次 Apply 产生了副作用:\n  %s", strings.Join(d, "\n  "))
	}
}

// requireClean 断言 Apply 之后 Read → Diff 为空。
func requireClean(t *testing.T, r Resource) {
	t.Helper()
	obs, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if obs.State != StatePresent {
		t.Fatalf("Apply 之后应当读到 present，实际 %s（%s）", obs.State, obs.Reason)
	}
	if d := r.Diff(obs); len(d) > 0 {
		t.Errorf("Apply 之后不应还有差异: %v", d)
	}
}

// requireOnlyField 断言差异恰好只涉及某一个字段。
func requireOnlyField(t *testing.T, r Resource, field string) {
	t.Helper()
	obs, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	changes := r.Diff(obs)
	if len(changes) != 1 {
		t.Fatalf("应当只报 %s 一处差异，实际 %d 处: %v", field, len(changes), changes)
	}
	if changes[0].Field != field {
		t.Errorf("报的是 %s，期望 %s", changes[0].Field, field)
	}
}

// requireAbsent 断言当前读到 absent。
func requireAbsent(t *testing.T, r Resource) {
	t.Helper()
	obs, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if obs.State != StateAbsent {
		t.Errorf("应当读到 absent，实际 %s", obs.State)
	}
}

// ── 命令替身 ────────────────────────────────────────────────────────────

// newFakeRunner 构造身份类资源测试用的替身：**未预设的查询一律按
// 「查无此项」作答**，未预设的变更按成功作答。
//
// 这个默认值是有意的——查询默认成功会让「用户不存在」的分支永远测不到。
func newFakeRunner() *command.Fake {
	f := command.NewFake()
	f.SetPrefix("getent ", command.Result{ExitCode: 2})
	f.SetPrefix("id ", command.Result{ExitCode: 2})
	return f
}
