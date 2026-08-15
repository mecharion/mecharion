package agent

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mecharion/mecharion/internal/pki"
	"github.com/mecharion/mecharion/internal/protocol"
)

// CertPaths 是本机证书的三个文件。
type CertPaths struct {
	Cert string
	Key  string
	CA   string
}

// NodeCertPaths 返回一个配置目录下的节点证书位置。
func NodeCertPaths(confDir string) CertPaths {
	dir := pki.Dir(confDir)
	return CertPaths{
		Cert: filepath.Join(dir, "node.crt"),
		Key:  filepath.Join(dir, "node.key"),
		CA:   filepath.Join(dir, "ca.crt"),
	}
}

// renewCerts 在证书快到期时换一张新的。
//
// **判断在 mechlet 侧做，不由 mechd 推**：证书在谁手上谁最清楚，而且这让
// 「节点断连三个月后回来」有明确行为——它一连上就发现自己过期了，走重新
// 加入而不是悄悄用一张废证书（22-multi-node §2.3）。
//
// 换完**不需要重启**：拨号用的 TLS 配置每次握手都从盘上读当前证书
// （见 mechletcmd.upstreamCreds）。旧连接继续用旧证书跑到它自己断开为止，
// 那没问题——旧证书此刻至少还有 30 天有效期。
func (a *Agent) renewCerts(ctx context.Context) {
	if a.opts.Certs.Cert == "" {
		return // 单机形态：走 unix socket，没有证书可续
	}
	left, err := certRemaining(a.opts.Certs.Cert)
	if err != nil {
		a.opts.Log.Warn("failed to read local certificate, skipping renewal check", "err", err)
		return
	}
	if left > a.renewBefore() {
		return
	}

	// 过期之后再续是没用的：那张证书连不上 mechd，而续期本身要走 mTLS。
	if left <= 0 {
		a.noteCertExpired(left)
		return
	}
	a.opts.Log.Info("certificate is nearing expiry, starting renewal",
		"remaining", left.Round(time.Hour).String(), "threshold", a.renewBefore().String())

	csrPEM, keyPEM, err := pki.NewNodeCSR(a.opts.Node)
	if err != nil {
		a.opts.Log.Error("failed to generate renewal certificate request", "err", err)
		return
	}
	cert, ca, err := a.opts.Client.RenewCert(ctx, csrPEM)
	if err != nil {
		a.opts.Log.Error("failed to renew certificate, will retry next round", "err", err)
		return
	}
	if err := writeCertSet(a.opts.Certs, cert, keyPEM, ca); err != nil {
		a.opts.Log.Error("failed to write new certificate", "err", err)
		return
	}
	a.opts.Log.Info("certificate renewed", "path", a.opts.Certs.Cert)
}

// noteCertExpired 在证书已经过期时喊一声，但只喊一次。
//
// 这是个**人必须介入**的状态：过期的证书连不上 mechd，而续期要走 mTLS。
// 每轮喊一次只会把日志淹掉，而它不会自己好转。
func (a *Agent) noteCertExpired(left time.Duration) {
	a.mu.Lock()
	first := !a.certExpired
	a.certExpired = true
	a.mu.Unlock()
	if !first {
		return
	}
	a.opts.Log.Error(
		"local certificate has expired, cannot auto-renew -- renewal itself requires a valid certificate",
		"expired", (-left).Round(time.Hour).String()+" ago",
		"next_step", "rejoin with a new join token: mechlet install --join ...")
}

func (a *Agent) renewBefore() time.Duration {
	if a.opts.RenewBefore > 0 {
		return a.opts.RenewBefore
	}
	return pki.RenewBefore
}

// certRemaining 返回一张证书还剩多久到期。
func certRemaining(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return 0, fmt.Errorf("%s 不是 PEM", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, err
	}
	return time.Until(cert.NotAfter), nil
}

// writeCertSet 原子地换掉三个文件。
//
// **先写临时文件再 rename**：拨号可能正在读它们，而半个证书比旧证书糟得多。
func writeCertSet(p CertPaths, cert, key, ca []byte) error {
	for _, f := range []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{p.Cert, cert, 0o644},
		{p.Key, key, 0o600},
		{p.CA, ca, 0o644},
	} {
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, f.body, f.mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// 编译期确认 Client 提供了续期入口。
var _ interface {
	RenewCert(context.Context, []byte) ([]byte, []byte, error)
} = (*protocol.Client)(nil)

// noteRefused 把「被控制面拒之门外」与「连不上」分开，并只喊一次。
//
// 上报失败的默认处理是 Debug 一句就算了——那对瞬时故障（mechd 正在重启、
// 网络抖动）是对的，mechlet 本就该继续自治。
//
// **但被吊销 / 被移除不是瞬时故障**：它不会自己好转，需要人介入。
// 沉默的代价是站在那台机器前的人**看不出它为什么不再同步**——中心侧
// 有一条「已吊销的节点尝试连接」，而节点侧什么都没有。
//
// 返回 true 表示这是一次拒绝，调用方不必再按普通失败处理。
func (a *Agent) noteRefused(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated:
	default:
		return false
	}

	a.mu.Lock()
	first := !a.refused
	a.refused = true
	a.mu.Unlock()

	if first {
		a.opts.Log.Error("control plane refused this node, sync stopped",
			"reason", status.Convert(err).Message(),
			"next_step", "once you've confirmed this machine is trusted, run mechctl node unrevoke, "+
				"or rejoin with a new token")
		return true
	}
	a.opts.Log.Debug("control plane is still refusing this node")
	return true
}

// clearRefused 在一次成功交互之后复位，让恢复之后的再次拒绝仍然喊得出来。
func (a *Agent) clearRefused() {
	a.mu.Lock()
	a.refused = false
	a.mu.Unlock()
}

// ── cordon ──────────────────────────────────────────────────────────────

// setCordoned 记下控制面下发的暂停状态，并在**解除**时喊一声。
//
// 进入时那一声由 noteCordoned 在调和循环里发（那时才知道它真的停了）；
// 解除时必须在这里发——恢复之后调和循环不再走那条分支，没人会说话。
//
// 两个方向都要有声音：运维 cordon 一台机器去调试，事后最想确认的
// 就是「它恢复了没有」。
func (a *Agent) setCordoned(v bool) {
	a.mu.Lock()
	lifted := a.cordoned && !v
	entered := !a.cordoned && v
	a.cordoned = v
	if lifted || entered {
		a.cordonAnnounced = false
	}
	a.mu.Unlock()

	if lifted {
		a.opts.Log.Warn("this node's reconcile pause was lifted, resuming normal reconcile")
	}
}

func (a *Agent) isCordoned() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cordoned
}

// noteCordoned 只在**进入**暂停状态时喊一声。
//
// 与「停在旧版」「被控制面拒绝」是同一条：稳定状态每轮播报一次，
// 只会把真正发生变化的那一刻淹掉。
func (a *Agent) noteCordoned(why string) {
	a.mu.Lock()
	first := !a.cordonAnnounced
	a.cordonAnnounced = true
	a.mu.Unlock()

	if first {
		a.opts.Log.Warn("this node's reconcile is paused (cordoned), desired state is still received but not applied",
			"resume", "mechctl node uncordon "+a.opts.Node)
		return
	}
	a.opts.Log.Debug("still in reconcile-paused state", "trigger", why)
}
