package protocol

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"crypto/tls"
)

// mtlsCtx 造一个「已经通过 mTLS 握手、CN 是 cn」的上下文。
//
// 填的是 VerifiedChains 而不是 PeerCertificates：那是 nodeOf 真正读的字段，
// 而两者的区别正是「验过什么」与「对端给了什么」。
func mtlsCtx(cn string) context.Context {
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{leaf}},
			},
		},
	})
}

// unixCtx 造一个没有 TLS 信息的上下文——unix socket 上就是这样。
func unixCtx() context.Context { return context.Background() }

// TestNodeIdentityPrefersCertificate 是 **M7 第 2 步的核心验收**，
// 钉住 ADR-0034 的第二条：多节点下身份以证书 CN 为准。
//
// 继续认请求里自报的 node_name，mTLS 就白做了——任何一张合法证书都能
// 冒充任何一个节点。
func TestNodeIdentityPrefersCertificate(t *testing.T) {
	cases := []struct {
		name    string
		ctx     context.Context
		claimed string
		want    string
		wantErr codes.Code
	}{
		{
			name: "unix socket 信自报的名字",
			ctx:  unixCtx(), claimed: "n1", want: "n1",
		},
		{
			name: "unix socket 上没有名字就没有身份",
			ctx:  unixCtx(), claimed: "", wantErr: codes.InvalidArgument,
		},
		{
			name: "mTLS 用证书的 CN",
			ctx:  mtlsCtx("n1"), claimed: "n1", want: "n1",
		},
		{
			name: "mTLS 下不自报名字也认得出",
			ctx:  mtlsCtx("n1"), claimed: "", want: "n1",
		},
		{
			// **这一条是整步的意义所在**
			name: "拿 n1 的证书自称 n2 被拒",
			ctx:  mtlsCtx("n1"), claimed: "n2", wantErr: codes.PermissionDenied,
		},
		{
			name: "证书里没有 CN 时拒绝",
			ctx:  mtlsCtx(""), claimed: "n1", wantErr: codes.Unauthenticated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nodeOf(tc.ctx, tc.claimed)
			if tc.wantErr != codes.OK {
				if status.Code(err) != tc.wantErr {
					t.Fatalf("错误码应为 %v，实际 %v（%v）", tc.wantErr, status.Code(err), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("身份 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestIdentityMismatchExplainsItself 钉住那条错误信息**说得清**。
//
// 「改了 --node 却没换证书」是这条规则最可能被触发的方式，而如果只回一句
// PermissionDenied，现场的人会去查网络和权限——那两处都是好的。
func TestIdentityMismatchExplainsItself(t *testing.T) {
	_, err := nodeOf(mtlsCtx("store-042"), "store-043")
	if err == nil {
		t.Fatal("应当拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"store-042", "store-043", "证书"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里应当有 %q，实际: %s", want, msg)
		}
	}
}

// TestUnixSocketStillTrustsClaimedName 钉住单机形态**没有被改变**。
//
// 多节点的代码不能让单机形态变复杂——单机是这个项目最主要的形态。
// 一个「顺手要求所有连接都带证书」的实现会让单机彻底不可用。
func TestUnixSocketStillTrustsClaimedName(t *testing.T) {
	got, err := nodeOf(unixCtx(), "standalone-box")
	if err != nil {
		t.Fatalf("unix socket 上不该要求证书: %v", err)
	}
	if got != "standalone-box" {
		t.Errorf("身份 = %q，期望 standalone-box", got)
	}
}
