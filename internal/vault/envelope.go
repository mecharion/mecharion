package vault

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是信封加密的**共用核心**：KEK 的读写、DEK 的包裹与解包。
//
// 抽出来是因为有两个使用者——mechd 的 Vault（值存 SQLite）与 mechlet 的
// FileVault（值存文件，ADR-0033）。**两套信封实现意味着两套边界条件**，
// 而其中一套的测试覆盖必然更薄：权限校验、密钥长度、AAD 绑定、
// 「换了一把 KEK」的报错——每一条都得在两边都对。
//
// 变的只有「密文存在哪」，因此只有那一层分成两份。

// LoadKEK 读主密钥并校验。
//
// 文件不存在时返回的错误满足 os.IsNotExist——调用方要据此决定是生成一把
// 新的还是拒绝启动，而那个判断两边不一样（mechd 看库里有没有密文，
// mechlet 看目录里有没有包裹过的 DEK）。
func LoadKEK(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// 权限过松等同于没加密——密钥与密文在同一台机器上，
	// 任何能读密钥的用户也能读到密文。
	if fi, serr := os.Stat(path); serr == nil && modeTooOpen(fi.Mode().Perm()) {
		return nil, fmt.Errorf(
			"vault: master key %s has permissions %04o, readable by more than its owner\n"+
				"  fix: chmod 0400 %s",
			path, fi.Mode().Perm(), path)
	}

	kek, err := hex.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, fmt.Errorf("vault: master key %s is not valid hexadecimal: %w", path, err)
	}
	if len(kek) != KeySize {
		return nil, fmt.Errorf("vault: master key %s is %d bytes, expected %d",
			path, len(kek), KeySize)
	}
	return kek, nil
}

// CreateKEK 生成主密钥并原子写入。
//
// 用十六进制而非裸二进制：密钥要被单独备份、可能经过工单与配置管理系统，
// 文本形式不会在传输中被字符集或换行转换弄坏。安全性无差别——两者对能读到
// 文件的人是一样的。
func CreateKEK(path string) ([]byte, error) {
	kek := make([]byte, KeySize)
	if _, err := rand.Read(kek); err != nil {
		return nil, fmt.Errorf("vault: generating master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(kek)+"\n"), 0o400); err != nil {
		return nil, fmt.Errorf("vault: writing master key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("vault: committing master key: %w", err)
	}
	return kek, nil
}

// NewDEK 生成一把数据密钥并用 KEK 包裹。
func NewDEK(kek []byte) (dek, wrapped []byte, err error) {
	kekAEAD, err := newAEAD(kek)
	if err != nil {
		return nil, nil, err
	}
	dek = make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("vault: generating data key: %w", err)
	}
	wrapped, err = seal(kekAEAD, dek, aadDEK)
	if err != nil {
		return nil, nil, err
	}
	return dek, wrapped, nil
}

// UnwrapDEK 用 KEK 解开数据密钥。
//
// 解不开最常见的原因是**换了一把主密钥**（比如从别处拷来的密钥文件）。
// 错误信息必须说这件事：底层的 "cipher: message authentication failed"
// 对排查毫无帮助。
func UnwrapDEK(kek, wrapped []byte, keyPath string) ([]byte, error) {
	kekAEAD, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	dek, err := open(kekAEAD, wrapped, aadDEK)
	if err != nil {
		return nil, fmt.Errorf(
			"vault: could not unwrap the data key with the current master key — key and "+
				"ciphertext don't match\n"+
				"  master key: %s\n"+
				"  this usually means they came from different backups", keyPath)
	}
	return dek, nil
}
