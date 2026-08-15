package protocol

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// nodeOf 是**协议层唯一的身份入口**：每个 RPC 都从这里拿到「对端是谁」。
//
// 它回答两个问题，而这两个问题必须分开：
//
//	你是谁      传输层事实（证书 CN 或 socket 权限），见下方的包级 nodeOf
//	你还能连吗  控制面状态（可能刚被吊销），见 authorize
//
// 合成一个会让「握手过了」被当成「获准了」——而应用层吊销的全部意义
// 恰恰是这两者可以不一致（ADR-0034）。
func (s *Server) nodeOf(ctx context.Context, claimed string) (string, error) {
	node, err := nodeOf(ctx, claimed)
	if err != nil {
		return "", err
	}
	// **吊销只对带证书的连接有意义。**
	//
	// unix socket 上没有证书可吊销，「谁能连」由 socket 权限回答
	// （ADR-0026）。在那里也查一遍，只会让一台被误删了节点行的单机
	// 彻底失联——而那台机器上的 mechd 就在本地。
	if _, mtls := peerCN(ctx); !mtls {
		return node, nil
	}
	if err := s.authorize(ctx, node); err != nil {
		return "", err
	}
	return node, nil
}

// authorize 查这个节点现在还能不能连。
//
// 被吊销的证书**握手仍会成功**——那是应用层吊销的代价，写在 ADR-0034 里。
// 因此这道门必须在业务层，且必须在**每个** RPC 上。
func (s *Server) authorize(ctx context.Context, node string) error {
	auth, ok := s.backend.(Authorizer)
	if !ok {
		return nil
	}
	if err := auth.Allowed(ctx, node); err != nil {
		s.noteRejected(node, err)
		return status.Errorf(codes.PermissionDenied, "%v", err)
	}
	return nil
}

// noteRejected 只在**第一次**拒绝时喊一声。
//
// agent 会以退避重连，被吊销之后每几秒一次——每次都写一条审计会把审计表
// 淹掉，而那张表的保留期以年计（07-persistence §1.4）。进入这个状态时说
// 一次就够了，「它一直在敲门」由日志的 Debug 级承载。
func (s *Server) noteRejected(node string, cause error) {
	s.mu.Lock()
	first := !s.rejected[node]
	if first {
		if s.rejected == nil {
			s.rejected = map[string]bool{}
		}
		s.rejected[node] = true
	}
	s.mu.Unlock()

	if first {
		s.log.Warn("a revoked node attempted to connect", "node", node, "reason", cause)
		return
	}
	s.log.Debug("revoked node is still retrying", "node", node)
}

// Authorizer 由知道「谁被吊销了」的 Backend 可选实现。
//
// 可选接口而不是塞进 Backend：协议层可以脱离控制面单独测，而那时
// 「有没有被吊销」这个问题不存在。
type Authorizer interface {
	// Allowed 报告某个节点现在是否还能连；不能时返回可展示的原因。
	Allowed(ctx context.Context, node string) error
}

// nodeOf 决定这次调用**自称/证实**是谁。
//
// 五个 RPC 都从请求里读得到 node_name，但那个字段只在一种传输上可信：
//
//	unix socket   没有证书可依，只能信自报的名字。这是成立的——
//	              socket 是 0600 root:root，能连上就已经是 root。
//	TCP + mTLS    以**证书 CN** 为准，忽略自报的名字。
//
// 多节点下继续认自报的名字，mTLS 就白做了：任何一张合法证书都能冒充
// 任何一个节点。
//
// 名字与 CN 不一致时**拒绝**，而不是静默以证书为准。静默会让一次配置
// 错误（改了 --node 却没换证书）表现成「节点名莫名其妙变回去了」，
// 那是最难查的一类现场。
func nodeOf(ctx context.Context, claimed string) (string, error) {
	cn, mtls := peerCN(ctx)
	if !mtls {
		if claimed == "" {
			return "", status.Error(codes.InvalidArgument, "缺少 node_name")
		}
		return claimed, nil
	}
	if cn == "" {
		// 走到这里说明握手过了却没有 CN——证书是本机 CA 签的，
		// 但没写身份。那是签发端的错，说清楚比默默放行有用。
		return "", status.Error(codes.Unauthenticated,
			"客户端证书里没有 CN——节点身份就是 CN（ADR-0034）")
	}
	if claimed != "" && claimed != cn {
		return "", status.Errorf(codes.PermissionDenied,
			"证书身份是 %q，但请求自称 %q——"+
				"多节点下以证书为准，不一致时拒绝。\n"+
				"  改了 --node 就要重新签一张证书", cn, claimed)
	}
	return cn, nil
}

// peerCN 取出对端证书的 CN；第二个返回值表示这条连接是不是 mTLS。
func peerCN(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", false
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}
	// VerifiedChains 非空说明证书**已经被校验过**——服务端配的是
	// RequireAndVerifyClientCert，因此走到这里必然非空。不直接读
	// PeerCertificates 是因为那是「对端给了什么」，不是「验过什么」。
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", true
	}
	return chains[0][0].Subject.CommonName, true
}

// CertRenewer 由能签发证书的 Backend 可选实现。
//
// 与 Authorizer 一样是可选接口：协议层可以脱离 PKI 单独测，
// 而那时「能不能续期」这个问题不存在。
type CertRenewer interface {
	// RenewCert 用一份 CSR 换一张新证书，返回 PEM。
	RenewCert(ctx context.Context, node string, csr []byte) (cert, ca []byte, err error)
}

// Cordoner 由知道「哪台机器被暂停了」的 Backend 可选实现。
//
// 与 Authorizer / CertRenewer 同一个模式：协议层可以脱离控制面单独测。
type Cordoner interface {
	// Cordoned 报告某个节点当前是否被暂停调和。
	Cordoned(ctx context.Context, node string) (bool, error)
}

// Purger 由知道「哪些孤儿该被清掉」的 Backend 可选实现。
type Purger interface {
	// PurgeOrphans 返回该节点上待清理的孤儿实例键。
	//
	// **返回的是键不是路径**：节点删的是自己本地收据里记的目录。
	// 中心下发绝对路径的话，一个同名重新部署过的组件会被误删。
	PurgeOrphans(ctx context.Context, node string) ([]string, error)
}
