package vault

import (
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FileVault 是 **mechlet 侧**的密钥保管：值存文件而不是 SQLite。
//
// 存在的理由见 [ADR-0033]：周期调和要在 mechd 不在时也能跑，而带
// `type: secret` 参数的规格里是哨兵串——没有明文就渲染不出配置。把明文
// 留在内存里意味着 mechlet 一重启就哑了，而**断连 + 重启正是最需要它的
// 时候**。
//
// 信封机制与 mechd 侧同一套（共用 envelope.go）：
//
//	主密钥 KEK   <dir>/kek        0400
//	数据密钥 DEK 被 KEK 包裹后存 <dir>/dek
//	值          被 DEK 加密后存 <dir>/secrets.json
//
// **边界要说清楚，别做安全剧场**：KEK 与密文在同一台机器上，因此它挡的
// 只有「磁盘被离线取走」「<data-dir> 被误同步/打包带走」这一类。**它挡不住
// 这台机器上在线的 root**——那种身份本来就能读到渲染出的配置文件，
// 里面有同样的口令。
type FileVault struct {
	dir      string
	keyPath  string
	aead     cipher.AEAD // nil 表示加密已关闭
	disabled bool
}

// FileOptions 控制 FileVault 的打开方式。
type FileOptions struct {
	// KeyPath 覆盖主密钥位置，缺省是 <dir>/kek。
	KeyPath string
	// Disabled 关闭加密，值以明文存盘（仍然 0600）。
	Disabled bool
}

// secretsFile 是值的落点。
const secretsFile = "secrets.json"

// dekFile 是被包裹的数据密钥。
const dekFile = "dek"

// OpenFile 打开（必要时创建）一个文件保管库。
func OpenFile(dir string, opts FileOptions) (*FileVault, error) {
	if dir == "" {
		return nil, fmt.Errorf("vault: missing vault directory")
	}
	// 0700：目录本身不可被他人列举。里面每个文件还会各自收紧。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("vault: creating vault directory: %w", err)
	}

	v := &FileVault{dir: dir, keyPath: opts.KeyPath, disabled: opts.Disabled}
	if v.keyPath == "" {
		v.keyPath = filepath.Join(dir, "kek")
	}

	if opts.Disabled {
		// 与 mechd 侧同一条纪律：拦住「原本加密过、现在关掉了」。
		// 不拦的话已有密文会被当成明文读出来，变成一串乱码口令下发给组件，
		// 而报错会出现在离根因很远的地方。
		if _, err := os.Stat(filepath.Join(dir, dekFile)); err == nil {
			return nil, fmt.Errorf(
				"vault: %s already has encrypted keys, encryption cannot be disabled now\n"+
					"  either restore the master key, or clear %s and let mechd resend everything", dir, dir)
		}
		return v, nil
	}

	kek, err := v.loadOrCreateKEK()
	if err != nil {
		return nil, err
	}
	dek, err := v.loadOrCreateDEK(kek)
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

func (v *FileVault) loadOrCreateKEK() ([]byte, error) {
	kek, err := LoadKEK(v.keyPath)
	switch {
	case err == nil:
		return kek, nil

	case os.IsNotExist(err):
		// 已有包裹过的 DEK 却没有 KEK：**拒绝**，不要偷偷生成一把新的。
		// 生成新的会让全部已存密钥变成解不开的字节，而症状是「组件拿到了
		// 空口令」——根因离现场极远。
		if _, derr := os.Stat(filepath.Join(v.dir, dekFile)); derr == nil {
			return nil, fmt.Errorf("%w\n"+
				"  expected location: %s\n"+
				"  these keys cannot be recovered. Clear %s and let mechd resend "+
				"everything — the node-side copy is just a cache, the source of truth is mechd",
				ErrKeyMissing, v.keyPath, v.dir)
		}
		return CreateKEK(v.keyPath)

	default:
		return nil, err // LoadKEK 已带上路径与原因
	}
}

func (v *FileVault) loadOrCreateDEK(kek []byte) ([]byte, error) {
	path := filepath.Join(v.dir, dekFile)

	wrapped, err := os.ReadFile(path)
	switch {
	case err == nil:
		return UnwrapDEK(kek, wrapped, v.keyPath)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("vault: reading data key %s: %w", path, err)
	}

	dek, wrapped, err := NewDEK(kek)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(path, wrapped); err != nil {
		return nil, fmt.Errorf("vault: writing data key: %w", err)
	}
	return dek, nil
}

// Enabled 报告加密是否开启。
func (v *FileVault) Enabled() bool { return v.aead != nil }

// KeyPath 返回主密钥路径。
func (v *FileVault) KeyPath() string { return v.keyPath }

// ── 值的读写 ────────────────────────────────────────────────────────────

// Get 取一条密钥的明文。
func (v *FileVault) Get(id string) (string, bool, error) {
	all, err := v.load()
	if err != nil {
		return "", false, err
	}
	blob, ok := all[id]
	if !ok {
		return "", false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", false, fmt.Errorf("vault: ciphertext for %s is not valid base64: %w", id, err)
	}
	plain, err := v.decrypt(raw, id)
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// Merge 写入若干条，保留其余。
//
// 用于增量下发：一次推送只带了部分实例的密钥，不能把别的实例的擦掉。
func (v *FileVault) Merge(values map[string]string) error {
	all, err := v.load()
	if err != nil {
		return err
	}
	for _, id := range sortedIDs(values) {
		blob, err := v.encrypt(values[id], id)
		if err != nil {
			return err
		}
		all[id] = base64.StdEncoding.EncodeToString(blob)
	}
	return v.save(all)
}

// Keep 删掉不在集合里的密钥。
//
// 组件被移除之后，它的口令留在盘上就是一份没有主人的凭据。
func (v *FileVault) Keep(ids map[string]bool) error {
	all, err := v.load()
	if err != nil {
		return err
	}
	changed := false
	for id := range all {
		if !ids[id] {
			delete(all, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return v.save(all)
}

// ── 落盘 ────────────────────────────────────────────────────────────────

func (v *FileVault) path() string { return filepath.Join(v.dir, secretsFile) }

func (v *FileVault) load() (map[string]string, error) {
	body, err := os.ReadFile(v.path())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vault: reading %s: %w", v.path(), err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("vault: parsing %s: %w", v.path(), err)
	}
	return out, nil
}

func (v *FileVault) save(all map[string]string) error {
	// encoding/json 对 map 按键排序，因此输出稳定——「这个文件变了没有」
	// 才是一个有意义的问题
	body, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("vault: serializing secrets: %w", err)
	}
	return writePrivate(v.path(), append(body, '\n'))
}

// writePrivate 以 0600 原子写入。
//
// 先写临时文件再 rename：中途断电不会留下半份密钥库——那会让 mechlet
// 下次启动时把一个截断的 JSON 当成「解析失败」，而真正的损失是全部密钥。
func writePrivate(path string, body []byte) error {
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── 加解密 ──────────────────────────────────────────────────────────────

// aadForID 把密文绑定到具体的密钥 id。
//
// 没有它，一条密文可以被从一个 id 搬到另一个——把测试组件的弱口令挪到
// 生产组件上。AEAD 的附加数据让这种挪动在解密时直接失败。
func aadForID(id string) []byte {
	return []byte("mecharion:node-secret:v1:" + id)
}

func (v *FileVault) encrypt(value, id string) ([]byte, error) {
	if v.aead == nil {
		return []byte(value), nil
	}
	return seal(v.aead, []byte(value), aadForID(id))
}

func (v *FileVault) decrypt(blob []byte, id string) (string, error) {
	if v.aead == nil {
		return string(blob), nil
	}
	plain, err := open(v.aead, blob, aadForID(id))
	if err != nil {
		return "", fmt.Errorf(
			"vault: decrypting %s failed — the ciphertext may be from a different master "+
				"key, or was moved here from another record\n"+
				"  the node-side key is just a cache: clear %s and let mechd resend it", id, v.dir)
	}
	return string(plain), nil
}

func sortedIDs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
