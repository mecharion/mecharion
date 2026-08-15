package vault

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/mecharion/mecharion/internal/pack"
)

// 各字符集的取值。
//
// 默认 alnum **排除符号**，是因为口令要穿过 shell、连接串、EnvironmentFile
// 与各家应用自己的解析器——符号是转义 bug 的主要来源（spec §7.6）。
// 确实需要更高熵时用 alnumSymbol，但要先确认消费方处理得了。
const (
	charsAlnum  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	charsSymbol = "!#%*+,-./:=?@^_~"
	charsHex    = "0123456789abcdef"
)

// charsetFor 返回字符集的可用字符。
func charsetFor(name string) (string, error) {
	switch name {
	case "", pack.CharsetAlnum:
		return charsAlnum, nil
	case pack.CharsetAlnumSymbol:
		return charsAlnum + charsSymbol, nil
	case pack.CharsetHex:
		return charsHex, nil
	default:
		return "", fmt.Errorf("vault: unknown charset %q", name)
	}
}

// Generate 为一个参数生成口令，**仅在它还没有值时**。
//
// 已有值（用户给的、或上次生成的）一律原样返回。每轮调和都重新生成会让
// 密码每 60 秒换一次，服务永远连不上——固化不是优化，是正确性（16-secrets §2）。
//
// 返回值 created 说明这次是不是真的生成了新值。
func (v *Vault) Generate(
	ctx context.Context, componentID int64, param string, g pack.Generate,
) (value string, version int, created bool, err error) {
	if cur, ver, ok, err := v.Get(ctx, componentID, param); err != nil {
		return "", 0, false, err
	} else if ok {
		return cur, ver, false, nil
	}

	value, err = randomString(g.EffectiveLength(), g.EffectiveCharset())
	if err != nil {
		return "", 0, false, err
	}
	version, err = v.Put(ctx, componentID, param, value)
	if err != nil {
		return "", 0, false, err
	}
	return value, version, true, nil
}

// Rotate 强制换一个新值，版本自增。
//
// 轮换必然产生新 generation：version 参与 spec digest，配置文件因而被重渲染、
// 服务被重启。这是**正确的**——文件内容确实变了（16-secrets §5）。
func (v *Vault) Rotate(
	ctx context.Context, componentID int64, param string, g pack.Generate,
) (string, int, error) {
	value, err := randomString(g.EffectiveLength(), g.EffectiveCharset())
	if err != nil {
		return "", 0, err
	}
	version, err := v.Put(ctx, componentID, param, value)
	if err != nil {
		return "", 0, err
	}
	return value, version, nil
}

// randomString 生成指定长度的随机串。
//
// **用 crypto/rand + 大整数取模，不用 `rand.Int63() % len`**：后者在字符集
// 长度不是 2 的幂时会让靠前的字符出现得更频繁。62 个字符的 alnum 正是这种
// 情况，偏置会实实在在地削减熵——而这种弱化不会有任何症状。
func randomString(length int, charset string) (string, error) {
	chars, err := charsetFor(charset)
	if err != nil {
		return "", err
	}
	if length < pack.MinGenerateLength {
		return "", fmt.Errorf("vault: generate length %d is below the minimum %d",
			length, pack.MinGenerateLength)
	}

	max := big.NewInt(int64(len(chars)))
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("vault: generating random number: %w", err)
		}
		out[i] = chars[n.Int64()]
	}
	return string(out), nil
}
