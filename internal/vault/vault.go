// Package vault 是 mechd 的密钥保管：生成、信封加密存储、轮换。
//
// 设计见 docs/design/16-secrets.md 与 ADR-0030。三层结构：
//
//	主密钥 KEK   /etc/mecharion/secret.key   0400，与数据库**分开备份**
//	数据密钥 DEK 被 KEK 包裹后存 vault_keys  换 KEK 只需重包这几十字节
//	值          被 DEK 加密后存 secrets      AES-256-GCM
//
// **明确的边界**：它挡「数据库副本外流」（备份、快照、支持包、误配的同步），
// **不挡 mechd 主机上的 root**——那种情况下两个文件都读得到。不做安全剧场。
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

// KeySize 是 KEK 与 DEK 的长度（AES-256）。
const KeySize = 32

// DefaultKeyPath 是主密钥的默认位置。
const DefaultKeyPath = "/etc/mecharion/secret.key"

// ErrKeyMissing 表示库里有密文但主密钥不在。
//
// 这种情况**必须拒绝启动**：静默把密钥当空值，会让所有依赖它的组件拿到
// 空口令并在启动时莫名其妙地失败，而根因在完全不相干的地方。
var ErrKeyMissing = errors.New("master key does not exist, but the database already has ciphertext")

// Options 控制 Vault 的打开方式。
type Options struct {
	// KeyPath 是主密钥文件路径。
	KeyPath string
	// Disabled 关闭加密，值以明文存库。
	//
	// 关掉之后数据库副本等同于口令副本——这条要让运维知道，
	// 而不是悄悄降级。
	Disabled bool
}

// Vault 持有解开后的数据密钥。
type Vault struct {
	store    *store.Store
	aead     cipher.AEAD // nil 表示加密已关闭
	keyPath  string
	disabled bool
}

// q 按 ctx 里有没有挂着 Deploy 那样的事务选出 Querier。
//
// **不能像以前那样固定绑死一个连接**：Vault 与 mechd 主库共用同一个
// SQLite（本就没有完全隔离——上面直接 import 了 sqlcgen），写连接全库
// 只有一个。Deploy 的写入被串成一个事务后，渲染管线里生成新
// 密钥会经这里问库；固定绑死的连接与 Deploy 当时占着的那个不是同一个，
// 于是问连接池再要一个，池子只有一个还被 Deploy 自己攥着，永远等不到，
// 直接卡死。改成每次现取——ctx 里有事务就用那个（与 Deploy 共用同一个
// 连接），没有就照旧用写连接池。
func (v *Vault) q(ctx context.Context) *sqlcgen.Queries { return v.store.WriteQueries(ctx) }

// Open 打开保管库：读 KEK、解开 DEK；首次使用时生成两者。
func Open(ctx context.Context, s *store.Store, opts Options) (*Vault, error) {
	v := &Vault{
		store:    s,
		keyPath:  opts.KeyPath,
		disabled: opts.Disabled,
	}
	if v.keyPath == "" {
		v.keyPath = DefaultKeyPath
	}
	if opts.Disabled {
		// 明文模式下仍要拦住「原本加密过、现在关掉了」——那会让已有密文
		// 变成读不出来的乱码，且错误信息出现在离根因很远的地方。
		if n, err := v.countSecrets(ctx); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, fmt.Errorf(
				"vault: the database already has %d ciphertext record(s), encryption cannot be disabled now\n"+
					"  first convert them to plaintext with mechctl secret decrypt-all, or restore the master key", n)
		}
		return v, nil
	}

	kek, err := v.loadOrCreateKEK(ctx)
	if err != nil {
		return nil, err
	}
	dek, err := v.loadOrCreateDEK(ctx, kek)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	v.aead = aead
	return v, nil
}

// loadOrCreateKEK 读主密钥；不存在且库里没有密文时生成一把。
func (v *Vault) loadOrCreateKEK(ctx context.Context) ([]byte, error) {
	kek, err := LoadKEK(v.keyPath)
	switch {
	case err == nil:
		return kek, nil

	case os.IsNotExist(err):
		// 库里已有东西却没有密钥 —— 拒绝，且说清楚后果
		hasDEK, derr := v.hasWrappedDEK(ctx)
		if derr != nil {
			return nil, derr
		}
		if hasDEK {
			return nil, fmt.Errorf("%w\n"+
				"  expected location: %s\n"+
				"  the master key and the database must be backed up separately (docs/design/07-persistence.md §1.7).\n"+
				"  if lost: generate-type secrets can be regenerated and redeployed;\n"+
				"           operator-supplied secrets cannot be recovered and must be retrieved from their source",
				ErrKeyMissing, v.keyPath)
		}
		return CreateKEK(v.keyPath)

	default:
		return nil, err // LoadKEK 已带上路径与原因
	}
}

// loadOrCreateDEK 取出被包裹的数据密钥；首次则生成并包裹。
func (v *Vault) loadOrCreateDEK(ctx context.Context, kek []byte) ([]byte, error) {
	row, err := v.q(ctx).GetWrappedDEK(ctx)
	if err == nil {
		return UnwrapDEK(kek, row.WrappedDek, v.keyPath)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("vault: reading data key: %w", err)
	}

	dek, wrapped, err := NewDEK(kek)
	if err != nil {
		return nil, err
	}
	if err := v.q(ctx).PutWrappedDEK(ctx, sqlcgen.PutWrappedDEKParams{
		WrappedDek: wrapped,
		CreatedAt:  store.FormatTime(v.store.Now()),
	}); err != nil {
		return nil, fmt.Errorf("vault: writing data key: %w", err)
	}
	return dek, nil
}

func (v *Vault) hasWrappedDEK(ctx context.Context) (bool, error) {
	_, err := v.q(ctx).GetWrappedDEK(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("vault: querying data key: %w", err)
	}
}

func (v *Vault) countSecrets(ctx context.Context) (int, error) {
	var n int
	err := v.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM secrets`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("vault: counting ciphertext: %w", err)
	}
	return n, nil
}

// Enabled 报告加密是否开启。
func (v *Vault) Enabled() bool { return v.aead != nil }

// KeyPath 返回主密钥路径，供安装时的备份提示使用。
func (v *Vault) KeyPath() string { return v.keyPath }

func modeTooOpen(m os.FileMode) bool { return permEnforced && m&0o077 != 0 }

// ── 值的读写 ────────────────────────────────────────────────────────────

// Secret 是一条密钥的元信息（不含值）。
type Secret struct {
	Param   string
	Version int
}

// Get 取出明文与版本。不存在时返回 (", 0, false, nil)。
func (v *Vault) Get(ctx context.Context, componentID int64, param string) (string, int, bool, error) {
	row, err := v.q(ctx).GetSecret(ctx, sqlcgen.GetSecretParams{
		ComponentID: componentID, Param: param,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: reading %s: %w", param, err)
	}

	plain, err := v.decrypt(row.Ciphertext, componentID, param)
	if err != nil {
		return "", 0, false, err
	}
	return plain, int(row.Version), true, nil
}

// Put 写入一个值。已存在时**版本自增**——version 参与 spec digest，
// 因此轮换必然产生新 generation（16-secrets §5）。
func (v *Vault) Put(ctx context.Context, componentID int64, param, value string) (int, error) {
	blob, err := v.encrypt(value, componentID, param)
	if err != nil {
		return 0, err
	}
	now := store.FormatTime(v.store.Now())
	row, err := v.q(ctx).PutSecret(ctx, sqlcgen.PutSecretParams{
		ComponentID: componentID, Param: param,
		Ciphertext: blob, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return 0, fmt.Errorf("vault: writing %s: %w", param, err)
	}
	return int(row.Version), nil
}

// List 列出某个 Component 的全部密钥元信息，**不含值**。
func (v *Vault) List(ctx context.Context, componentID int64) ([]Secret, error) {
	rows, err := v.q(ctx).ListSecrets(ctx, componentID)
	if err != nil {
		return nil, fmt.Errorf("vault: listing secrets: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, Secret{Param: r.Param, Version: int(r.Version)})
	}
	return out, nil
}

// ── 加解密 ──────────────────────────────────────────────────────────────

// aadDEK 是包裹数据密钥时的附加数据。
var aadDEK = []byte("mecharion:dek:v1")

// aadFor 把密文绑定到具体的 (component, param)。
//
// 没有它，一条密文可以被从一行搬到另一行——把测试库里的弱口令挪到生产
// 组件上，或者把低权限账号的口令挪成 superuser 的。AEAD 的附加数据让这种
// 挪动在解密时直接失败。
func aadFor(componentID int64, param string) []byte {
	return []byte(fmt.Sprintf("mecharion:secret:v1:%d:%s", componentID, param))
}

func (v *Vault) encrypt(value string, componentID int64, param string) ([]byte, error) {
	if v.aead == nil {
		return []byte(value), nil
	}
	return seal(v.aead, []byte(value), aadFor(componentID, param))
}

func (v *Vault) decrypt(blob []byte, componentID int64, param string) (string, error) {
	if v.aead == nil {
		return string(blob), nil
	}
	plain, err := open(v.aead, blob, aadFor(componentID, param))
	if err != nil {
		return "", fmt.Errorf("vault: decrypting %s failed — the ciphertext may be from a "+
			"different key, or was moved here from another record", param)
	}
	return string(plain), nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: constructing block cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: constructing AEAD: %w", err)
	}
	return aead, nil
}

// seal 输出 nonce ‖ 密文。
func seal(aead cipher.AEAD, plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: generating nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plain, aad), nil
}

func open(aead cipher.AEAD, blob, aad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(blob) < n {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, blob[:n], blob[n:], aad)
}
