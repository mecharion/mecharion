package protocol

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gateBackend 是一个只回答「能不能连」的假 Backend。
type gateBackend struct {
	Backend
	denied map[string]error
	asked  int
}

func (g *gateBackend) Allowed(_ context.Context, node string) error {
	g.asked++
	return g.denied[node]
}

func newGateServer(denied map[string]error) (*Server, *gateBackend) {
	b := &gateBackend{denied: denied}
	s := NewServer(ServerOptions{
		Backend: b,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return s, b
}

// TestRevokedNodeIsRejectedOnMTLS 是 **M7 第 5 步的核心验收**。
//
// 应用层吊销的全部前提是：被吊销的证书**握手仍会成功**（ADR-0034）。
// 因此这道门必须在业务层，且必须在每个 RPC 上——漏一个就是一条能绕过
// 吊销的路。
func TestRevokedNodeIsRejectedOnMTLS(t *testing.T) {
	s, b := newGateServer(map[string]error{
		"gone": errors.New("节点 gone 的证书已被吊销"),
	})

	if _, err := s.nodeOf(mtlsCtx("gone"), "gone"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("被吊销的节点应当被拒，实际 %v", err)
	}
	if b.asked == 0 {
		t.Error("应当问过 Backend")
	}
	// 没被吊销的照常放行
	if got, err := s.nodeOf(mtlsCtx("ok"), "ok"); err != nil || got != "ok" {
		t.Errorf("未吊销的节点应当放行，实际 %q / %v", got, err)
	}
}

// TestRevocationDoesNotTouchUnixSocket 钉住单机形态**没有被改变**。
//
// unix socket 上没有证书可吊销，「谁能连」由 socket 权限回答。
// 在那里也查一遍，只会让一台被误删了节点行的单机彻底失联——
// 而那台机器上的 mechd 就在本地。
func TestRevocationDoesNotTouchUnixSocket(t *testing.T) {
	s, b := newGateServer(map[string]error{
		"standalone-box": errors.New("不该被问到"),
	})
	got, err := s.nodeOf(unixCtx(), "standalone-box")
	if err != nil {
		t.Fatalf("unix socket 上不该查吊销: %v", err)
	}
	if got != "standalone-box" {
		t.Errorf("身份 = %q", got)
	}
	if b.asked != 0 {
		t.Errorf("unix socket 上不该问 Backend，实际问了 %d 次", b.asked)
	}
}

// TestRejectionIsAnnouncedOnce 钉住「进入时喊一声，之后安静」。
//
// agent 会以退避重连，被吊销之后每几秒一次。每次都写一条审计会把审计表
// 淹掉，而那张表的保留期以年计。
func TestRejectionIsAnnouncedOnce(t *testing.T) {
	s, _ := newGateServer(map[string]error{"gone": errors.New("已吊销")})

	for i := 0; i < 5; i++ {
		_, _ = s.nodeOf(mtlsCtx("gone"), "gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.rejected["gone"] {
		t.Error("应当记住这个节点已经喊过了")
	}
}

// TestRejectionExplainsWhy 钉住那句拒绝**说得清**。
//
// 「不在册」与「已吊销」是两件事：前者可能是被 remove 了或从没加入过，
// 后者是有人主动切断的。现场第一个要知道的正是「是哪一种」。
func TestRejectionExplainsWhy(t *testing.T) {
	s, _ := newGateServer(map[string]error{
		"gone": errors.New("节点 gone 的证书已于 2026-01-01 被吊销"),
	})
	_, err := s.nodeOf(mtlsCtx("gone"), "gone")
	if err == nil {
		t.Fatal("应当拒绝")
	}
	if !strings.Contains(err.Error(), "吊销") {
		t.Errorf("错误信息应当说清是被吊销了，实际: %v", err)
	}
}
