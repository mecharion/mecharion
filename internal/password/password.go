// Package password 做口令的哈希与校验。
//
// 用 **argon2id**：它同时抗 GPU（内存硬）与抗侧信道，是 OWASP 现在的首选，
// 也是 [ADR-0037] 定的。
//
// [ADR-0037]: ../../docs/adr/0037-login-is-full-privilege.md
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 默认参数。
//
// **参数会连同摘要一起存进编码串**，因此这里调强不需要数据迁移：老口令用
// 它们当初的参数验，下次改口令时自然升到新的。这正是 argon2 编码格式存在的
// 理由，也是不要自己拼「盐 + 摘要」两列的理由。
const (
	// defaultMemory 是 64 MiB。OWASP 给 argon2id 的建议下限之一
	// （m=47104, t=1, p=1）比这个低，这里取更保守的一档。
	defaultMemory = 64 * 1024
	// defaultTime 是迭代次数。
	defaultTime = 3
	// defaultKeyLen 是摘要长度。
	defaultKeyLen = 32
	// saltLen 是盐长度。16 字节是 argon2 规范的建议值。
	saltLen = 16
)

// ErrMismatch 表示口令不对。
//
// **不区分「用户不存在」与「口令错」**：区分了就等于给攻击者一个用户名
// 枚举接口。调用方一律回同一句话。
var ErrMismatch = errors.New("用户名或口令不正确")

// params 是一次哈希用到的代价参数。
type params struct {
	memory  uint32
	time    uint32
	threads uint8
	keyLen  uint32
}

func defaults() params {
	// 并行度取 CPU 数，但封顶——一台 64 核的机器上取 64 会让每次登录
	// 都占满 CPU，而那是个能被利用的放大器。
	t := runtime.NumCPU()
	if t > 4 {
		t = 4
	}
	if t < 1 {
		t = 1
	}
	return params{
		memory: defaultMemory, time: defaultTime,
		threads: uint8(t), keyLen: defaultKeyLen,
	}
}

// Hash 返回口令的 argon2id 编码串。
func Hash(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("口令不能为空")
	}
	p := defaults()
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐: %w", err)
	}
	sum := argon2.IDKey([]byte(plain), salt, p.time, p.memory, p.threads, p.keyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		b64(salt), b64(sum)), nil
}

// Verify 校验口令。
//
// **总是走完整个哈希**，即使编码串一看就不对：提前返回会让「这个用户存在
// 吗」变成一个计时问题。
func Verify(encoded, plain string) error {
	p, salt, want, err := parse(encoded)
	if err != nil {
		// 编码串坏了也照样烧掉一次哈希的时间，再回同一句话
		_ = argon2.IDKey([]byte(plain), make([]byte, saltLen),
			defaultTime, defaultMemory, 1, defaultKeyLen)
		return ErrMismatch
	}
	got := argon2.IDKey([]byte(plain), salt, p.time, p.memory, p.threads, p.keyLen)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash 报告这个编码串是不是用旧参数算的。
//
// 调用方可以在**验证成功之后**顺手用新参数重算一遍——那是唯一能拿到明文的
// 时刻。不做的话，调强参数只对新用户生效。
func NeedsRehash(encoded string) bool {
	p, _, _, err := parse(encoded)
	if err != nil {
		return true
	}
	d := defaults()
	return p.memory != d.memory || p.time != d.time || p.keyLen != d.keyLen
}

func parse(encoded string) (p params, salt, sum []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("不是 argon2id 编码串")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, errors.New("读不出版本")
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("argon2 版本 %d 不受支持", version)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, errors.New("读不出代价参数")
	}
	if salt, err = unb64(parts[4]); err != nil {
		return p, nil, nil, errors.New("盐不是合法 base64")
	}
	if sum, err = unb64(parts[5]); err != nil {
		return p, nil, nil, errors.New("摘要不是合法 base64")
	}
	p.keyLen = uint32(len(sum))
	return p, salt, sum, nil
}

func b64(b []byte) string            { return base64.RawStdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }
