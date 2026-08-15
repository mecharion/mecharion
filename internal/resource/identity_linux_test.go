//go:build linux

package resource

import (
	"context"
	"os"
	"testing"
)

// 命令替身能验证「该不该动手」，但验证不了「flag 名字对不对」——
// `usermod --home-dir` 是拼错的（usermod 用 --home，只有 useradd 用
// --home-dir），而替身对任何参数都答成功。
//
// 这几个用例跑真正的 useradd/groupadd，因此要求 root 与 Linux。
// 在 test/node 的 systemd 容器里跑（make testbin && hack/testenv.sh）。
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能建用户；在 test/node 容器里跑")
	}
}

func realEnv(t *testing.T) *Env {
	t.Helper()
	env := testEnv(t)
	env.Runner = ExecRunner{}
	return env
}

func TestRealUserCreateAndConverge(t *testing.T) {
	requireRoot(t)
	env := realEnv(t)
	ctx := context.Background()
	const name = "m7ntest"

	// 上次跑剩下的残留
	_, _ = ExecRunner{}.Run(ctx, "userdel", name)
	t.Cleanup(func() { _, _ = ExecRunner{}.Run(ctx, "userdel", name) })

	u := build(t, env, mk(t, "user:"+name, TypeUser, map[string]any{
		"name": name, "system": true, "shell": "/usr/sbin/nologin",
	}))

	requireAbsent(t, u)
	if err := u.Apply(ctx); err != nil {
		t.Fatalf("useradd 失败——检查 flag 拼写: %v", err)
	}

	obs, err := u.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != StatePresent {
		t.Fatalf("建完应当读到 present，实际 %s（%s）", obs.State, obs.Reason)
	}
	if obs.Field("shell") != "/usr/sbin/nologin" {
		t.Errorf("shell = %q，期望 /usr/sbin/nologin", obs.Field("shell"))
	}
	if d := u.Diff(obs); len(d) > 0 {
		t.Errorf("建完不应还有差异: %v", d)
	}

	// 幂等：第二次 Apply 不该再调 useradd（会以「已存在」失败）
	if err := u.Apply(ctx); err != nil {
		t.Fatalf("第二次 Apply 应当无操作: %v", err)
	}

	// usermod 路径：换 shell
	u2 := build(t, env, mk(t, "user:"+name, TypeUser, map[string]any{
		"name": name, "shell": "/bin/sh",
	}))
	requireOnlyField(t, u2, "shell")
	if err := u2.Apply(ctx); err != nil {
		t.Fatalf("usermod 失败——检查 flag 拼写: %v", err)
	}
	requireClean(t, u2)
}

func TestRealGroupCreate(t *testing.T) {
	requireRoot(t)
	env := realEnv(t)
	ctx := context.Background()
	const name = "m7ngrp"

	_, _ = ExecRunner{}.Run(ctx, "groupdel", name)
	t.Cleanup(func() { _, _ = ExecRunner{}.Run(ctx, "groupdel", name) })

	g := build(t, env, mk(t, "group:"+name, TypeGroup, map[string]any{
		"name": name, "system": true,
	}))
	requireAbsent(t, g)
	if err := g.Apply(ctx); err != nil {
		t.Fatalf("groupadd 失败——检查 flag 拼写: %v", err)
	}
	requireClean(t, g)
	if err := g.Apply(ctx); err != nil {
		t.Fatalf("第二次 Apply 应当无操作: %v", err)
	}
}

// TestRealOwnershipApplied 验证 owner/group 真的落到了文件上。
func TestRealOwnershipApplied(t *testing.T) {
	requireRoot(t)
	env := realEnv(t)
	ctx := context.Background()
	const name = "m7nown"

	_, _ = ExecRunner{}.Run(ctx, "userdel", name)
	t.Cleanup(func() { _, _ = ExecRunner{}.Run(ctx, "userdel", name) })
	if err := mustRun(ctx, ExecRunner{}, "建用户", "useradd", "--system", name); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	path := root + "/conf/app.yaml"
	f := build(t, env, mk(t, "file:app", TypeFile, map[string]any{
		"path": path, "content": "port: 8080\n",
		"owner": name, "group": name, "mode": "0640",
	}))

	requireIdempotent(t, f, root)
	requireClean(t, f)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, _, ok := fileOwner(fi)
	if !ok {
		t.Fatal("Linux 上应当能读到 uid")
	}
	if got := env.NameForID(ctx, "passwd", uid); got != name {
		t.Errorf("属主 = %s，期望 %s", got, name)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("权限 = %04o，期望 0640", fi.Mode().Perm())
	}

	// 手工改坏属主后，Diff 只该报 owner
	if err := os.Chown(path, 0, 0); err != nil {
		t.Fatal(err)
	}
	obs, _ := f.Read(ctx)
	changes := f.Diff(obs)
	if len(changes) != 2 {
		t.Fatalf("应当报 owner 与 group 两处，实际 %v", changes)
	}
	if err := f.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	requireClean(t, f)
}

// TestRealGetentUnknownUser 验证真实 getent 的「查无此项」退出码是 2。
//
// 这条约定是三态判定的基础：退出码 2 是 Absent，其它非零是 Unknown。
func TestRealGetentUnknownUser(t *testing.T) {
	res, err := ExecRunner{}.Run(context.Background(), "getent", "passwd", "绝无此人")
	if err != nil {
		t.Skipf("本机没有 getent: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("getent 查无此项的退出码 = %d，期望 2——"+
			"这条约定变了会让 Absent 被误判成 Unknown", res.ExitCode)
	}
}
