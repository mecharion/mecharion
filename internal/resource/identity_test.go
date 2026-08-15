package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
)

// 身份类资源用命令替身测试：真实的 useradd 需要 root，而幂等与
// 「读不到时不动手」这两条契约与是否真的建了用户无关。
// 端到端验证在 systemd 容器里做（test/node）。

func idEnv(t *testing.T) (*Env, *command.Fake) {
	t.Helper()
	fr := newFakeRunner()
	env := testEnv(t)
	env.Runner = fr
	return env, fr
}

func TestUserCreatedWhenAbsent(t *testing.T) {
	env, fr := idEnv(t)
	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{
		"name": "webapp", "system": true,
	}))

	requireAbsent(t, u)
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fr.Ran("useradd --system webapp") {
		t.Errorf("应当调用 useradd --system，实际执行了: %v", fr.Calls())
	}
}

// TestUserApplyTwiceIsNoop 是 §6 强制要求的幂等用例。
func TestUserApplyTwiceIsNoop(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd webapp", CmdResult{
		Stdout: "webapp:x:997:997::/nonexistent:/usr/sbin/nologin\n",
	})

	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{
		"name": "webapp", "system": true,
	}))
	ctx := context.Background()
	if err := u.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if fr.Ran("useradd") || fr.Ran("usermod") {
		t.Errorf("用户已存在且无差异时不该改动系统，实际执行了: %v", fr.Calls())
	}
	requireClean(t, u)
}

// TestUserUnknownDoesNotCreate 钉住「读不出来就不动手」。
//
// 把 Unknown 当成 Absent 会让引擎去 useradd 一个在 LDAP 里已经存在的名字。
func TestUserUnknownDoesNotCreate(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd webapp", CmdResult{ExitCode: 3, Stderr: "nsswitch 超时"})

	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{"name": "webapp"}))

	obs, err := u.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != StateUnknown {
		t.Fatalf("getent 异常应当读成 unknown，实际 %s", obs.State)
	}
	if obs.Reason == "" {
		t.Error("unknown 必须说明为什么读不到")
	}
	if d := u.Diff(obs); d != nil {
		t.Error("unknown 不该产生差异——它不是漂移，是读不到")
	}

	err = u.Apply(context.Background())
	if err == nil {
		t.Fatal("读不出来时不该动手")
	}
	if !IsTransient(err) {
		t.Errorf("读不到应归为 transient（下轮再试），实际 %s", ClassOf(err))
	}
	if fr.Ran("useradd") {
		t.Error("绝不能在 unknown 状态下 useradd")
	}
}

// TestUserUIDMismatchRefuses 钉住「不自动改 uid」。
func TestUserUIDMismatchRefuses(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd webapp", CmdResult{
		Stdout: "webapp:x:997:997::/nonexistent:/usr/sbin/nologin\n",
	})

	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{
		"name": "webapp", "uid": 1500,
	}))

	requireOnlyField(t, u, "uid")

	err := u.Apply(context.Background())
	if err == nil {
		t.Fatal("uid 不一致时必须拒绝——改 uid 会让已有文件变成孤儿属主")
	}
	if ClassOf(err) != ErrPermanent {
		t.Errorf("应归为 permanent，实际 %s", ClassOf(err))
	}
	if fr.Ran("usermod") {
		t.Error("不该执行 usermod")
	}
}

// TestUserConvergesShell 钉住「可以安全改的字段确实会被收敛」。
func TestUserConvergesShell(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd webapp", CmdResult{
		Stdout: "webapp:x:997:997::/nonexistent:/bin/sh\n",
	})

	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{
		"name": "webapp", "shell": "/usr/sbin/nologin",
	}))
	requireOnlyField(t, u, "shell")

	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fr.Ran("usermod --shell /usr/sbin/nologin webapp") {
		t.Errorf("应当 usermod 收敛 shell，实际: %v", fr.Calls())
	}
}

// TestUserSupplementaryGroupsUseAppend 钉住「附加组用 --append」。
//
// 覆盖式的 --groups 会把别的组件加进去的组一并抹掉。
func TestUserConvergesGroupsWithAppend(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd hdfs", CmdResult{
		Stdout: "hdfs:x:996:996::/var/lib/hdfs:/bin/bash\n",
	})
	fr.Set("getent group 996", CmdResult{Stdout: "hdfs:x:996:\n"})
	fr.Set("id -nG hdfs", CmdResult{Stdout: "hdfs monitoring\n"})

	u := build(t, env, mk(t, "user:hdfs", TypeUser, map[string]any{
		"name": "hdfs", "groups": []string{"hdfs", "hadoop"},
	}))
	requireOnlyField(t, u, "groups")

	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fr.Ran("usermod --append --groups hdfs,hadoop hdfs") {
		t.Errorf("应当用 --append 收敛附加组，实际: %v", fr.Calls())
	}
}

// TestUserRemoveIsNoop 钉住「删组件不删用户」。
func TestUserRemoveIsNoop(t *testing.T) {
	env, fr := idEnv(t)
	u := build(t, env, mk(t, "user:webapp", TypeUser, map[string]any{"name": "webapp"}))
	if err := u.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls()) != 0 {
		t.Errorf("Remove 必须是 no-op——删掉用户会让已有文件变成孤儿 uid，"+
			"而新用户复用该 uid 时会意外获得这些文件的所有权。实际执行了: %v", fr.Calls())
	}
}

func TestGroupLifecycle(t *testing.T) {
	env, fr := idEnv(t)
	g := build(t, env, mk(t, "group:webapp", TypeGroup, map[string]any{
		"name": "webapp", "system": true,
	}))

	requireAbsent(t, g)
	if err := g.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fr.Ran("groupadd --system webapp") {
		t.Errorf("应当调用 groupadd --system，实际: %v", fr.Calls())
	}

	// 已存在后再 Apply 不该有动作
	fr.Set("getent group webapp", CmdResult{Stdout: "webapp:x:997:\n"})
	fr.Reset()
	if err := g.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fr.Ran("groupadd") || fr.Ran("groupmod") {
		t.Errorf("已存在时不该有动作，实际: %v", fr.Calls())
	}
	requireClean(t, g)

	if err := g.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGroupGIDMismatchRefuses(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent group webapp", CmdResult{Stdout: "webapp:x:997:\n"})

	g := build(t, env, mk(t, "group:webapp", TypeGroup, map[string]any{
		"name": "webapp", "gid": 1500,
	}))
	requireOnlyField(t, g, "gid")

	if err := g.Apply(context.Background()); err == nil {
		t.Fatal("gid 不一致时必须拒绝")
	}
}

func TestIdentityRejectsBadArgs(t *testing.T) {
	env, _ := idEnv(t)
	for _, typ := range []string{TypeUser, TypeGroup} {
		t.Run(typ, func(t *testing.T) {
			_, err := New(env, mk(t, typ+":x", typ, map[string]any{"system": true}))
			if err == nil || !strings.Contains(err.Error(), "缺少 name") {
				t.Errorf("期望「缺少 name」，实际: %v", err)
			}
		})
	}
}

// TestUIDZeroIsDeclarable 钉住「uid: 0 是已声明而非未声明」。
func TestUIDZeroIsDeclarable(t *testing.T) {
	env, fr := idEnv(t)
	fr.Set("getent passwd svc", CmdResult{Stdout: "svc:x:997:997::/x:/bin/sh\n"})

	u := build(t, env, mk(t, "user:svc", TypeUser, map[string]any{
		"name": "svc", "uid": 0,
	}))
	requireOnlyField(t, u, "uid")
}
