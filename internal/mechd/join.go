package mechd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pki"
	"github.com/mecharion/mecharion/internal/store"
)

// JoinTokenPrefix 让一个泄漏的 join token 一眼可辨，也与 admin token
// 区分开——两者权限完全不同，混用时的错误信息必须说得清是哪一种。
const JoinTokenPrefix = "m7n_join_"

// DefaultJoinTTL 是 join token 的默认有效期。
//
// 30 分钟：它的用途是「运维现在正在装这台机器」。更长没有意义，
// 而**一张躺在聊天记录里三天的 token 是一个真实的风险**（ADR-0034 里
// 写明了这条路没有第二道人工闸门）。
const DefaultJoinTTL = 30 * time.Minute

// JoinTokenView 是 token 的展示形态。
//
// **Token 字段只在创建那一刻有值**：库里存的是哈希，列表永远看不到明文。
type JoinTokenView struct {
	ID      int64  `json:"id"`
	Token   string `json:"token,omitempty"`
	Node    string `json:"node,omitempty"`
	Expires string `json:"expires"`
	MaxUses int    `json:"maxUses"`
	Used    int    `json:"used"`
	Revoked bool   `json:"revoked,omitempty"`
	// Usable 与 Reason 把三种失效分开说：过期、用尽、已吊销是三件不同
	// 的事，而现场最先要知道的正是「是哪一种」。
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
	// JoinCommand 是可以直接粘贴的加入命令，含 CA 公钥指纹。
	JoinCommand string `json:"joinCommand,omitempty"`
}

// CreateJoinToken 生成一张加入凭据。
//
// 明文**只在这里返回一次**，之后任何接口都拿不到——与 admin token 同一条
// 纪律（08-security §3.3）。
func (s *Service) CreateJoinToken(
	ctx context.Context, siteName, node string, ttl time.Duration, uses int,
	confDir, actor string,
) (JoinTokenView, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return JoinTokenView{}, err
	}
	if ttl <= 0 {
		ttl = DefaultJoinTTL
	}
	if uses <= 0 {
		uses = 1
	}
	if node == "" && uses == 1 {
		// 不绑名 + 只能用一次是最常见的误配：它既没有绑名的安全性，
		// 又没有多次可用的便利。不拦，但要在返回里说清楚。
		s.log().Debug("created an unbound single-use token", "site", site.Name)
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return JoinTokenView{}, err
	}
	plain := JoinTokenPrefix + hex.EncodeToString(buf)

	now := s.now()
	t, err := s.Repos.JoinTokens().Create(ctx, store.JoinToken{
		SiteID: site.ID, Hash: HashToken(plain), NodeName: node,
		ExpiresAt: now.Add(ttl), MaxUses: uses,
		CreatedAt: now, CreatedBy: actor,
	})
	if err != nil {
		return JoinTokenView{}, err
	}
	s.audit(ctx, actor, "join-token-create", node, nil, site.Name)

	v := joinTokenView(t, now)
	v.Token = plain
	if hash, err := pki.CAHash(pki.Dir(confDir)); err == nil {
		v.JoinCommand = fmt.Sprintf(
			"mechlet install --join https://<mechd-host>:8443 \\\n"+
				"    --token %s \\\n    --ca-hash %s", plain, hash)
	}
	return v, nil
}

// ListJoinTokens 列出本 Site 的凭据（不含明文）。
func (s *Service) ListJoinTokens(ctx context.Context, siteName string) ([]JoinTokenView, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	list, err := s.Repos.JoinTokens().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]JoinTokenView, 0, len(list))
	for _, t := range list {
		out = append(out, joinTokenView(t, now))
	}
	return out, nil
}

// RevokeJoinToken 吊销一张凭据。
func (s *Service) RevokeJoinToken(
	ctx context.Context, siteName string, id int64, actor string,
) error {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return err
	}
	list, err := s.Repos.JoinTokens().List(ctx, site.ID)
	if err != nil {
		return err
	}
	for _, t := range list {
		if t.ID != id {
			continue
		}
		if err := s.Repos.JoinTokens().Revoke(ctx, id, s.now()); err != nil {
			return err
		}
		s.audit(ctx, actor, "join-token-revoke", fmt.Sprint(id), nil, site.Name)
		return nil
	}
	return faults.Permanentf("revoking join token", "no credential with id=%d in this Site", id)
}

func joinTokenView(t store.JoinToken, now time.Time) JoinTokenView {
	usable, reason := t.Usable(now)
	return JoinTokenView{
		ID: t.ID, Node: t.NodeName, Expires: store.FormatTime(t.ExpiresAt),
		MaxUses: t.MaxUses, Used: t.Used, Revoked: t.RevokedAt != nil,
		Usable: usable, Reason: reason,
	}
}

// ── join ────────────────────────────────────────────────────────────────

// JoinRequest 是一台机器请求加入。
type JoinRequest struct {
	Token string
	Node  string
	// CSR 是节点本机生成的证书请求。**私钥不过网**。
	CSR     []byte
	Address string
	ConfDir string
}

// JoinResult 是签发结果。
type JoinResult struct {
	Node string
	Cert []byte
	CA   []byte
}

// Join 用一张凭据换一份节点证书。
//
// **这是整条链路上唯一不需要既有身份的入口**（22-multi-node §3）。
// 因此这里的每一条校验都是那道门本身：
//
//	token 存在、未过期、未用尽、未吊销
//	绑名的 token 只能签出该名字
//	不能顶替一个已经在册的名字
//
// 顺序是刻意的：先校验 token 再看名字。反过来的话，一个拿着废 token 的人
// 能通过「这个名字被占了没」的报错**探测集群里有哪些节点**。
func (s *Service) Join(ctx context.Context, req JoinRequest) (*JoinResult, error) {
	if req.Token == "" {
		return nil, faults.Permanentf("joining cluster", "missing token")
	}
	t, err := s.Repos.JoinTokens().GetByHash(ctx, HashToken(req.Token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 不说「不存在」——那会把「token 打错了」与「token 没被建过」
			// 区分开，而两者对调用方是同一件事：这张 token 不管用。
			return nil, faults.Permanentf("joining cluster", "invalid token")
		}
		return nil, err
	}
	if ok, why := t.Usable(s.now()); !ok {
		return nil, faults.Permanentf("joining cluster", "token %s", why)
	}

	node := req.Node
	switch {
	case t.NodeName != "" && node == "":
		// 绑名的 token 可以不自报名字，用绑的那个
		node = t.NodeName
	case t.NodeName != "" && node != t.NodeName:
		return nil, faults.Permanentf("joining cluster",
			"this token is bound to node name %q, it cannot be used to join %q", t.NodeName, node)
	case node == "":
		return nil, faults.Permanentf("joining cluster",
			"this token has no bound node name, --node must be specified when joining")
	}

	site, err := s.Repos.Sites().Get(ctx, t.SiteID)
	if err != nil {
		return nil, err
	}

	// **不能顶替一个已经在册、且发过证书的名字。** 在册意味着它可能正在
	// 跑着组件，而一张新证书会让另一台机器开始以它的身份上报——两台
	// 机器抢一个身份，状态视图会在两者之间跳。
	//
	// **`reserved` 是例外**：那是 `node add` 预登记的位置，从未签发过
	// 证书，「同一身份两张有效证书」的风险在这里不成立
	// （22-multi-node §6.16）——放行它，让下面的签发与 ClaimNode 把它
	// 从「预登记」变成「已认领」，这正是批量预置场景需要的那条路。
	//
	// 这次检查只是给常见的非并发情况一个友好、指名道姓的错误；它本身
	// 不提供并发安全——真正的把关在下面 ClaimNode 的原子事务里，见那里
	// 的注释。这里过了不代表一定抢得到。
	if existing, err := s.Repos.Nodes().GetByName(ctx, site.ID, node); err == nil {
		if !existing.Reserved() || existing.Revoked() {
			return nil, faults.Permanentf("joining cluster",
				"node %s is already registered. to replace it with a different machine, first run mechctl node remove %s",
				node, node)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	cert, ca, err := pki.SignNodeCSR(pki.Dir(req.ConfDir), node, req.CSR)
	if err != nil {
		return nil, faults.Permanentf("joining cluster", "issuing certificate: %v", err)
	}

	// **先签发再落库**是不行的，反过来也不行——两者之间崩溃总会留下不一致。
	// 选「签发在前、落库在后」：多一张没人用的证书是无害的（它对应的节点
	// 不在册，任何 RPC 都会被拒），而多一行没有证书的节点会让运维以为
	// 那台机器已经加入了。
	//
	// token 消耗与节点落库现在是**同一个事务**：上面的存在性
	// 检查与 Usable 判断都只是「快路径」，真正防并发的是 ClaimNode。
	if _, err := s.Repos.ClaimNode(ctx, t.ID, store.Node{
		SiteID: site.ID, Name: node, Address: req.Address, Status: store.NodePending,
	}, s.now()); err != nil {
		switch {
		case errors.Is(err, store.ErrTokenUnavailable):
			return nil, faults.Permanentf("joining cluster",
				"token is no longer usable (it may be exhausted, expired, revoked, or already claimed by a concurrent join request)")
		case errors.Is(err, store.ErrNodeTaken):
			return nil, faults.Permanentf("joining cluster",
				"node %s is already registered. to replace it with a different machine, first run mechctl node remove %s",
				node, node)
		}
		return nil, err
	}

	s.audit(ctx, "join:"+node, "node-join", node, nil, site.Name)
	s.event(ctx, site.ID, "node.joined", node, map[string]any{
		"address": req.Address, "token": t.ID,
	})
	s.log().Info("node joined", "node", node, "site", site.Name, "token", t.ID)

	return &JoinResult{Node: node, Cert: cert, CA: ca}, nil
}

// ── 吊销与续期 ──────────────────────────────────────────────────────────

// SetNodeRevoked 吊销或恢复一个节点的证书。
//
// **与 `node remove` 是两件事**：remove 把节点从册子上抹掉（换硬件、
// 退役），revoke 保留那一行但切断它——「这台机器被偷了/被攻破了，
// 先断掉，但我还要看它上面装过什么」。
func (s *Service) SetNodeRevoked(
	ctx context.Context, siteName, name string, revoked bool, actor string,
) error {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return err
	}
	node, err := s.Repos.Nodes().GetByName(ctx, site.ID, name)
	if err != nil {
		return err
	}
	var at *time.Time
	action := "node-unrevoke"
	if revoked {
		now := s.now()
		at, action = &now, "node-revoke"
	}
	if err := s.Repos.Nodes().SetRevoked(ctx, node.ID, at); err != nil {
		return err
	}
	s.audit(ctx, actor, action, name, nil, site.Name)
	s.event(ctx, site.ID, "node."+strings.TrimPrefix(action, "node-"), name, nil)
	return nil
}

// RenewCert 用一份新的 CSR 换一张新证书。
//
// **调用方的身份已经由 mTLS 确定**（协议层的 nodeOf），因此这里不需要
// 任何凭据——现有证书就是身份证明。这与 join 的方向相反：那边是
// 「还没有身份的机器要一个」，这边是「已有身份的机器换一张」。
//
// 仍然要走 Allowed：一台刚被吊销的机器不该能给自己续命。协议层已经查过
// 一次，这里不重复——重复查会让「哪一层负责授权」变得含糊。
func (s *Service) RenewCert(
	ctx context.Context, node string, csr []byte, confDir string,
) (cert, ca []byte, err error) {
	cert, ca, err = pki.SignNodeCSR(pki.Dir(confDir), node, csr)
	if err != nil {
		return nil, nil, faults.Permanentf("renewing certificate", "%v", err)
	}
	site, serr := s.resolveSite(ctx, "")
	if serr == nil {
		s.audit(ctx, "node:"+node, "node-cert-renew", node, nil, site.Name)
	}
	s.log().Info("certificate renewed for node", "node", node)
	return cert, ca, nil
}

// SetNodeCordoned 暂停或恢复一个节点的调和。
//
// **与 revoke / remove 都不同**：那两个切断的是「能不能连」，cordon 保留
// 连接、保留上报，只让调和停下来——「我要手工调试这台机，别让 mechlet
// 把我的改动改回去」（10-cli §4.2）。
//
// 改完立刻唤醒那个节点：cordon 是**状态**，随下一次全量下发送达。
// 不唤醒的话它要等到下一次期望状态变化才知道，而那可能是几天以后。
func (s *Service) SetNodeCordoned(
	ctx context.Context, siteName, name string, cordoned bool, actor string,
) error {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return err
	}
	node, err := s.Repos.Nodes().GetByName(ctx, site.ID, name)
	if err != nil {
		return err
	}
	var at *time.Time
	action := "node-uncordon"
	if cordoned {
		now := s.now()
		at, action = &now, "node-cordon"
	}
	if err := s.Repos.Nodes().SetCordoned(ctx, node.ID, at); err != nil {
		return err
	}
	s.audit(ctx, actor, action, name, nil, site.Name)
	s.event(ctx, site.ID, "node."+strings.TrimPrefix(action, "node-"), name, nil)
	if s.Notify != nil {
		s.Notify.Notify(name)
	}
	return nil
}
