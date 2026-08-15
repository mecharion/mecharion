package ctlcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mecharion/mecharion/internal/mechd"
)

// 这一组测的是 `component remove` 的**流程**，不是某个函数的判读。
//
// 早先这里用的是「指向一个连不上的地址」那套办法（rollout 的确认测试就是
// 那样写的）。对 remove **行不通**：这条命令会先干跑一次算影响面，于是
// 请求在确认之前就失败了——测试看起来绿，实际一次都没走到确认。
// 那正是「失败了 ≠ 因为我想要的原因失败」的典型。
//
// 因此这里起一个真的 HTTP 桩：干跑那次能成功返回，流程才走得到确认那一步。
// 桩同时**记下每一次真删的请求**——判据是「有没有把删除发出去」，
// 而不是「命令有没有报错」。

// stubMechd 是一个只认 DELETE /components/{name} 的假 mechd。
type stubMechd struct {
	srv *httptest.Server
	mu  sync.Mutex
	// destructive 是收到的**非干跑**请求，也就是真的要删东西的那些。
	destructive []mechd.RemoveBody
}

func newStubMechd(t *testing.T, impact mechd.RemovalImpact) *stubMechd {
	t.Helper()
	s := &stubMechd{}
	s.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var body mechd.RemoveBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !body.DryRun {
				s.mu.Lock()
				s.destructive = append(s.destructive, body)
				s.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mechd.RemoveResult{
				Impact: impact, DryRun: body.DryRun,
			})
		}))
	t.Cleanup(s.srv.Close)
	return s
}

// deleted 报告桩有没有收到过一次真正的删除请求。
func (s *stubMechd) deleted() []mechd.RemoveBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mechd.RemoveBody(nil), s.destructive...)
}

func (s *stubMechd) runRemove(
	t *testing.T, stdin string, args ...string,
) (string, error) {
	t.Helper()
	root := newMechctlRoot(NewComponentCmd)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append(append([]string{"component"}, args...),
		"--server", s.srv.URL, "--token", "m7n_stub-token"))
	err := root.Execute()
	return out.String() + errBuf.String(), err
}

func sampleImpact() mechd.RemovalImpact {
	return mechd.RemovalImpact{
		Component: "pg-main", Pack: "postgresql", Version: "16.4",
		Nodes: []string{"n1", "n2"}, Instances: 2,
		Deleted:  []string{"/opt/mecharion/apps/pg-main", "/etc/mecharion/apps/pg-main"},
		Retained: []string{"/var/lib/mecharion/apps/pg-main"},
	}
}

// ── 确认这一关真的挡得住 ────────────────────────────────────────────────

// TestRemoveSendsNothingDestructiveWithoutTheName 是这一组的核心。
//
// 判据是**桩有没有收到真删的请求**，不是命令有没有报错：一个先删后问的
// 实现同样会报错（问的时候用户说了不），但东西已经没了。
func TestRemoveSendsNothingDestructiveWithoutTheName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin string
		args  []string
	}{
		{"什么都不输入", "", nil},
		{"输错名字", "pg-mian\n", nil},
		{"回答 y 不算数", "y\n", nil},
		{"-y 也跳不过", "", []string{"-y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubMechd(t, sampleImpact())
			out, err := s.runRemove(t, tc.stdin,
				append([]string{"remove", "pg-main"}, tc.args...)...)
			if err == nil {
				t.Fatalf("应当被确认拦下:\n%s", out)
			}
			if got := s.deleted(); len(got) > 0 {
				t.Fatalf("确认没通过却已经把删除发出去了: %+v", got)
			}
		})
	}
}

// TestRemoveGoesThroughWithTheName：输对了名字就该放行。
//
// 反面必须成立，否则上面那组只是「这条命令永远失败」。
func TestRemoveGoesThroughWithTheName(t *testing.T) {
	s := newStubMechd(t, sampleImpact())
	out, err := s.runRemove(t, "pg-main\n", "remove", "pg-main")
	if err != nil {
		t.Fatalf("输对了名字应当放行: %v\n%s", err, out)
	}
	got := s.deleted()
	if len(got) != 1 {
		t.Fatalf("应当发出恰好一次删除，实际 %d 次", len(got))
	}
	// 确认串要带上去——服务端还要再验一遍
	if got[0].Confirm != "pg-main" {
		t.Errorf("请求体里的 confirm = %q，期望组件名", got[0].Confirm)
	}
}

// TestPurgeDataNeedsItsOwnConfirmation 是 10-cli §7 表里那一行：
// `component remove --purge-data` 需要**组件名 ＋ 单独确认删除数据**。
//
// 删掉进程与配置可以重新部署回来，删掉数据不能——一个动作不该同时买断
// 这两者。
func TestPurgeDataNeedsItsOwnConfirmation(t *testing.T) {
	s := newStubMechd(t, sampleImpact())
	// 过了第一档（名字），第二档不回答
	out, err := s.runRemove(t, "pg-main\n", "remove", "pg-main", "--purge-data")
	if err == nil {
		t.Fatalf("只过了第一档不该放行:\n%s", out)
	}
	if got := s.deleted(); len(got) > 0 {
		t.Fatalf("第二档没拦住，数据被删了: %+v", got)
	}
}

func TestPurgeDataGoesThroughWhenBothAnswered(t *testing.T) {
	s := newStubMechd(t, sampleImpact())
	out, err := s.runRemove(t, "pg-main\ny\n", "remove", "pg-main", "--purge-data")
	if err != nil {
		t.Fatalf("两档都过了应当放行: %v\n%s", err, out)
	}
	got := s.deleted()
	if len(got) != 1 || !got[0].PurgeData {
		t.Fatalf("应当带着 purgeData 发出去，实际 %+v", got)
	}
}

// ── 干跑 ────────────────────────────────────────────────────────────────

// TestDryRunAsksNothingAndDeletesNothing：--dry-run 不该弹任何确认。
//
// 它存在的意义正是**让人在确认之前看清后果**。要求先确认再预览是把顺序
// 反过来了，而那会让人为了看一眼后果而先答应删除。
func TestDryRunAsksNothingAndDeletesNothing(t *testing.T) {
	s := newStubMechd(t, sampleImpact())
	out, err := s.runRemove(t, "", "remove", "pg-main", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run 不该需要任何确认: %v\n%s", err, out)
	}
	if got := s.deleted(); len(got) > 0 {
		t.Fatalf("--dry-run 把删除发出去了: %+v", got)
	}
	// 影响面要真的打出来
	for _, want := range []string{"2 instance", "will delete", "will keep",
		"/var/lib/mecharion/apps/pg-main"} {
		if !strings.Contains(out, want) {
			t.Errorf("影响面里应当有 %q:\n%s", want, out)
		}
	}
}

// TestImpactIsPrintedBeforeAsking 钉住顺序：先看清后果，再被问。
//
// 反过来的话，人是在不知道要删几台机器的情况下打下那个名字的——
// 而那正是这一档想要防的。
func TestImpactIsPrintedBeforeAsking(t *testing.T) {
	s := newStubMechd(t, sampleImpact())
	out, _ := s.runRemove(t, "", "remove", "pg-main")
	if !strings.Contains(out, "About to remove pg-main") {
		t.Fatalf("被拒绝之前也应当已经把影响面打出来:\n%s", out)
	}
	if !strings.Contains(out, "n1 n2") {
		t.Errorf("要说清是哪几台机器:\n%s", out)
	}
}
