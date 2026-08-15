package resource

import (
	"context"
	"os/user"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/spec"
)

// TestGetentFallbackByNameAndID 钉住「按名字查和按 id 反查走不同的函数」。
//
// 这里曾经有过一个 bug：反查用的是 user.Lookup（按名字），传进去一个
// 数字自然查不到，于是每个声明了 owner 的文件都会报一次永远收敛不了的
// 属主漂移——Apply 明明成功了，下一轮 Diff 还是报差异。
func TestGetentFallbackByNameAndID(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skipf("取不到当前用户: %v", err)
	}

	byName, err := getentFallback("passwd", me.Username, false)
	if err != nil {
		t.Fatalf("按名字查失败: %v", err)
	}
	if len(byName) < 3 || byName[2] != me.Uid {
		t.Fatalf("按名字查 %s 得到 %v，期望 uid %s", me.Username, byName, me.Uid)
	}

	byID, err := getentFallback("passwd", me.Uid, true)
	if err != nil {
		t.Fatalf("按 uid 反查失败: %v", err)
	}
	if len(byID) == 0 || byID[0] != me.Username {
		t.Fatalf("按 uid %s 反查得到 %v，期望名字 %s", me.Uid, byID, me.Username)
	}
}

// TestGetentFallbackUnknownIsNotError 钉住「查无此项返回 (nil, nil)」。
//
// 这条区分决定了三态：查无此项是 Absent，查询失败才是 Unknown。
func TestGetentFallbackUnknownIsNotError(t *testing.T) {
	ent, err := getentFallback("passwd", "m7n-绝无此人-zzz", false)
	if err != nil {
		t.Errorf("查无此项不该是错误（那会被判成 Unknown 而永远跳过）: %v", err)
	}
	if ent != nil {
		t.Errorf("查无此项应返回 nil，实际 %v", ent)
	}
}

func TestGetentFallbackRejectsUnknownDB(t *testing.T) {
	if _, err := getentFallback("hosts", "x", false); err == nil {
		t.Error("不支持的 NSS 数据库应当报错")
	}
}

// TestNameForIDFallsBackToNumber 钉住「反查不到时退回数字，而不是空串」。
//
// 退回空串会让差异显示成 `owner: → webapp`，看不出实际是什么。
func TestNameForIDFallsBackToNumber(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd 65533", CmdResult{ExitCode: 2})

	if got := env.NameForID(context.Background(), "passwd", 65533); got != "65533" {
		t.Errorf("反查不到应返回数字本身，实际 %q", got)
	}
}

// TestNameForIDIsCached 钉住「反查结果被缓存」。
//
// readInto 每个文件都会调它，不缓存就是每个文件 fork 一次 getent。
func TestNameForIDIsCached(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd 997", CmdResult{Stdout: "webapp:x:997:997::/x:/bin/sh\n"})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if got := env.NameForID(ctx, "passwd", 997); got != "webapp" {
			t.Fatalf("第 %d 次反查 = %q", i, got)
		}
	}
	n := 0
	for _, c := range fr.Calls() {
		if c == "getent passwd 997" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("反查应当只执行一次，实际 %d 次", n)
	}
}

// TestLookupUIDAcceptsNumericName 钉住「owner 直接写数字也能用」。
func TestLookupUIDAcceptsNumericName(t *testing.T) {
	env, fr := idEnv(t)
	got, err := env.LookupUID(context.Background(), "1000")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1000 {
		t.Errorf("LookupUID(\"1000\") = %d", got)
	}
	if len(fr.Calls()) != 0 {
		t.Errorf("纯数字不该再去查 NSS，实际执行了: %v", fr.Calls())
	}
}

func TestLookupUIDReportsMissing(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd 查无此人", CmdResult{ExitCode: 2})

	_, err := env.LookupUID(context.Background(), "查无此人")
	if err == nil {
		t.Fatal("解析不存在的用户名应当报错")
	}
	if ClassOf(err) != ErrPermanent {
		t.Errorf("owner 写了个不存在的用户是配置错误，应归为 permanent，实际 %s", ClassOf(err))
	}
}

func TestLookupGIDCaches(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent group webapp", CmdResult{Stdout: "webapp:x:997:\n"})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, err := env.LookupGID(ctx, "webapp")
		if err != nil {
			t.Fatal(err)
		}
		if got != 997 {
			t.Fatalf("LookupGID = %d，期望 997", got)
		}
	}
	n := 0
	for _, c := range fr.Calls() {
		if c == "getent group webapp" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("正查也应当缓存，实际执行了 %d 次", n)
	}
}

func TestExecRunnerCapturesExitCode(t *testing.T) {
	// 退出码不是 error —— getent 的 2 是正常结果，不该被当成失败
	res, err := ExecRunner{}.Run(context.Background(), "m7n-这个命令不存在-zzz")
	if err == nil {
		t.Error("命令不存在应当返回 error")
	}
	_ = res
}

// TestEnvForIndexesBlobs 钉住「NewEnv 把 blob 列表转成按名字索引」。
func TestEnvForIndexesBlobs(t *testing.T) {
	s := &spec.ResolvedSpec{Blobs: []spec.BlobRef{
		{Name: "main", SHA256: "aa", Size: 10},
		{Name: "extra", SHA256: "bb", Size: 20},
	}}
	env := EnvFor(s, "/var/lib/mecharion/packs/go-webapp", "/var/lib/mecharion/blobs")

	got, err := env.Blob("main")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != "aa" {
		t.Errorf("blob main = %+v", got)
	}

	_, err = env.Blob("没这个")
	if err == nil {
		t.Fatal("未声明的 blob 应当报错")
	}
	// 错误信息要列出可选项——名字打错时这是最有用的一行
	for _, want := range []string{"main", "extra"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应列出已声明的 blob，缺 %q: %v", want, err)
		}
	}
}
