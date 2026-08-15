package password

import (
	"errors"
	"strings"
	"testing"
)

// TestRoundTrip 钉住最基本的那条。
func TestRoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(h, "correct horse battery staple"); err != nil {
		t.Errorf("正确的口令应当通过: %v", err)
	}
	if err := Verify(h, "correct horse battery stapl"); !errors.Is(err, ErrMismatch) {
		t.Errorf("错误的口令应当被拒，实际 %v", err)
	}
}

// TestSaltIsRandom 钉住同一个口令每次哈希都不同。
//
// 盐固定的话，两个用同一口令的用户在库里长得一模一样——**攻击者一眼就能
// 看出谁和谁口令相同**，而且一张彩虹表能同时打穿他们。
func TestSaltIsRandom(t *testing.T) {
	a, _ := Hash("same")
	b, _ := Hash("same")
	if a == b {
		t.Fatal("同一个口令两次哈希结果相同——盐没有随机")
	}
	if err := Verify(a, "same"); err != nil {
		t.Error(err)
	}
	if err := Verify(b, "same"); err != nil {
		t.Error(err)
	}
}

// TestEncodedCarriesParams 钉住参数写在串里。
//
// **这是「将来调强参数不用迁移」的全部依据**：老口令用它们当初的参数验。
// 参数只写在代码里的话，一次调强会让所有老用户登不进来。
func TestEncodedCarriesParams(t *testing.T) {
	h, err := Hash("x")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$argon2id$", "v=19", "m=", "t=", "p="} {
		if !strings.Contains(h, want) {
			t.Errorf("编码串里缺 %q: %s", want, h)
		}
	}

	p, _, _, err := parse(h)
	if err != nil {
		t.Fatalf("自己产出的串应当解析得了: %v", err)
	}
	if p.memory != defaultMemory || p.time != defaultTime {
		t.Errorf("解出来的参数与默认不符: %+v", p)
	}
}

// TestVerifyRejectsGarbage 钉住坏掉的编码串不会 panic，也不会误放行。
func TestVerifyRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$aa$bb",
		"$argon2id$v=99$m=1,t=1,p=1$aa$bb",  // 版本不认识
		"$argon2id$v=19$m=1,t=1,p=1$!!!$bb", // 盐不是 base64
	} {
		if err := Verify(bad, "whatever"); !errors.Is(err, ErrMismatch) {
			t.Errorf("坏串 %q 应当回 ErrMismatch，实际 %v", bad, err)
		}
	}
}

// TestWholeDigestIsCompared 钉住比的是**整个**摘要，不是前几个字节。
//
// 这条是追一个存活的变异补的：把比较改成只看第一个字节，上面那些测试
// **一条都没红**——因为随机的错误口令几乎总在第一个字节就不同（1/256 才
// 碰撞）。而那个改动实际上把强度从 2^256 砍到了 2^8。
//
// 判据因此不能是「错口令被拒」，得是「**摘要的任何一位被改动都会被发现**」。
func TestWholeDigestIsCompared(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	_, _, sum, err := parse(h)
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.Join(strings.Split(h, "$")[:5], "$")

	// 首、中、尾各翻一位——只比前缀的实现会漏掉后两个
	for _, i := range []int{0, len(sum) / 2, len(sum) - 1} {
		tampered := append([]byte(nil), sum...)
		tampered[i] ^= 0x01

		bad := prefix + "$" + b64(tampered)
		if err := Verify(bad, pw); !errors.Is(err, ErrMismatch) {
			t.Errorf("摘要第 %d 位被改动却仍然通过——比较没有覆盖整个摘要", i)
		}
	}

	// 而没被动过的那份仍然要通过（否则上面那条可能只是「什么都不通过」）
	if err := Verify(h, pw); err != nil {
		t.Fatalf("未被改动的摘要应当通过: %v", err)
	}
}

// TestMismatchMessageDoesNotLeakWhichPartFailed 钉住错误信息不区分。
//
// 「用户不存在」与「口令错」说成两句话，等于白送一个用户名枚举接口。
func TestMismatchMessageDoesNotLeakWhichPartFailed(t *testing.T) {
	msg := ErrMismatch.Error()
	for _, leak := range []string{"不存在", "没有这个", "口令错误"} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误信息 %q 泄漏了是哪一半不对", msg)
		}
	}
	if !strings.Contains(msg, "或") {
		t.Errorf("错误信息应当把两种情况并成一句，实际 %q", msg)
	}
}

// TestEmptyPasswordRefused 钉住空口令建不出来。
func TestEmptyPasswordRefused(t *testing.T) {
	if _, err := Hash(""); err == nil {
		t.Fatal("空口令应当被拒绝")
	}
}

// TestNeedsRehash 钉住参数变了认得出来。
func TestNeedsRehash(t *testing.T) {
	h, _ := Hash("x")
	if NeedsRehash(h) {
		t.Error("刚用默认参数算的，不该需要重算")
	}
	weak := "$argon2id$v=19$m=8,t=1,p=1$YWFhYWFhYWFhYWFhYWFhYQ$YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE"
	if !NeedsRehash(weak) {
		t.Error("参数明显偏弱的串应当被认出来")
	}
	if !NeedsRehash("garbage") {
		t.Error("坏串应当当成需要重算")
	}
}
