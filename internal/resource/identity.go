package resource

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/mecharion/mecharion/internal/spec"
)

// `user` 与 `group` 的类型名。
const (
	TypeUser  = "user"
	TypeGroup = "group"
)

// IdentityName 取一条 user / group 资源声明的名字，其余类型返回空串。
//
// 卸载时的 `--purge-user` 需要知道「这个 Pack 建过哪些用户」，而那时
// 不该、也不能构造出 Resource 实例：Read 会去查 NSS，而我们只是要个名字。
//
// 解不出来返回空串而不是报错：一条参数畸形的资源在正常调和路径上早就
// 失败了，走到卸载还遇上它，跳过比中止整条删除路径合理。
func IdentityName(r spec.Resource) string {
	var a struct {
		Name string `json:"name"`
	}
	if r.Type != TypeUser && r.Type != TypeGroup {
		return ""
	}
	if err := json.Unmarshal(r.Args, &a); err != nil {
		return ""
	}
	return a.Name
}

type userArgs struct {
	Name string `json:"name"`
	// UID 是指针：0 是合法 uid（root），不能用零值表示「未声明」。
	UID    *int     `json:"uid,omitempty"`
	Group  string   `json:"group,omitempty"`
	Groups []string `json:"groups,omitempty"`
	Home   string   `json:"home,omitempty"`
	Shell  string   `json:"shell,omitempty"`
	System bool     `json:"system,omitempty"`
}

// User 保证一个系统用户存在。
type User struct {
	base
	env  *Env
	args userArgs
}

func newUser(env *Env, r spec.Resource) (Resource, error) {
	var a userArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}
	if a.Name == "" {
		return nil, badArg(r, "缺少 name")
	}
	return &User{base: base{id: r.ID, typ: r.Type}, env: env, args: a}, nil
}

// Read 查 NSS。
//
// getent 超时或 nsswitch 异常 → Unknown 而非 Absent。这条区分很重要：
// 当成 Absent 会让引擎去 useradd 一个在 LDAP 里已经存在的名字。
func (u *User) Read(ctx context.Context) (Observed, error) {
	ent, err := getent(ctx, u.env.runner(), "passwd", u.args.Name)
	if err != nil {
		return unknown("查询用户 %s: %v", u.args.Name, err), nil
	}
	if ent == nil {
		return Observed{State: StateAbsent}, nil
	}
	if len(ent) < 7 {
		return unknown("getent passwd %s 的输出无法解析", u.args.Name), nil
	}

	fields := map[string]any{
		"uid":   ent[2],
		"gid":   ent[3],
		"home":  ent[5],
		"shell": ent[6],
	}
	if gid, cerr := strconv.Atoi(ent[3]); cerr == nil {
		fields["primaryGroup"] = u.env.NameForID(ctx, "group", gid)
	}
	if len(u.args.Groups) > 0 {
		g, gerr := supplementaryGroups(ctx, u.env.runner(), u.args.Name)
		if gerr != nil {
			return unknown("查询 %s 的附加组: %v", u.args.Name, gerr), nil
		}
		fields["groups"] = g
	}
	return present(fields), nil
}

// Diff 只比较已声明的字段。
func (u *User) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}

	if u.args.UID != nil {
		b.scalar("uid", strconv.Itoa(*u.args.UID), o.Field("uid"))
	}
	b.scalar("primaryGroup", u.args.Group, o.Field("primaryGroup"))
	b.scalar("home", u.args.Home, o.Field("home"))
	b.scalar("shell", u.args.Shell, o.Field("shell"))
	if len(u.args.Groups) > 0 {
		got, _ := o.Fields["groups"].([]string)
		b.list("groups", u.args.Groups, got)
	}
	return b.changes
}

// Apply 建用户，或把已存在的用户收敛到声明。
func (u *User) Apply(ctx context.Context) error {
	obs, err := u.Read(ctx)
	if err != nil {
		return err
	}
	switch obs.State {
	case StateUnknown:
		// 读不出来就不动手——盲目 useradd 可能撞上 LDAP 里的同名用户
		return Transient("创建用户", errUnknownIdentity(u.args.Name, obs.Reason))
	case StateAbsent:
		return u.create(ctx)
	default:
		return u.converge(ctx, obs)
	}
}

func (u *User) create(ctx context.Context) error {
	args := []string{}
	if u.args.System {
		args = append(args, "--system")
	}
	if u.args.UID != nil {
		args = append(args, "--uid", strconv.Itoa(*u.args.UID))
	}
	if u.args.Group != "" {
		args = append(args, "--gid", u.args.Group)
	}
	if len(u.args.Groups) > 0 {
		args = append(args, "--groups", strings.Join(u.args.Groups, ","))
	}
	if u.args.Home != "" {
		args = append(args, "--home-dir", u.args.Home)
	}
	if u.args.Shell != "" {
		args = append(args, "--shell", u.args.Shell)
	}
	args = append(args, u.args.Name)
	return mustRun(ctx, u.env.runner(), "创建用户", "useradd", args...)
}

// converge 用 usermod 收敛可以安全改动的字段。
func (u *User) converge(ctx context.Context, obs Observed) error {
	// uid 变更**不自动执行**：系统里可能有大量文件以旧 uid 为属主，
	// 改掉用户的 uid 会让它们一次性变成孤儿。这是运维动作，必须由人做。
	if u.args.UID != nil && strconv.Itoa(*u.args.UID) != obs.Field("uid") {
		return Permanentf("收敛用户",
			"用户 %s 已存在且 uid 为 %s，声明要求 %d\n"+
				"  改 uid 会让已有文件变成孤儿属主，不自动执行；"+
				"请手工处理后重试",
			u.args.Name, obs.Field("uid"), *u.args.UID)
	}

	var args []string
	if u.args.Group != "" && u.args.Group != obs.Field("primaryGroup") {
		args = append(args, "--gid", u.args.Group)
	}
	if u.args.Home != "" && u.args.Home != obs.Field("home") {
		args = append(args, "--home", u.args.Home)
	}
	if u.args.Shell != "" && u.args.Shell != obs.Field("shell") {
		args = append(args, "--shell", u.args.Shell)
	}
	if len(u.args.Groups) > 0 {
		got, _ := obs.Fields["groups"].([]string)
		if !sameSet(u.args.Groups, got) {
			// --append 而非覆盖：用户可能被别的组件也加进了某个组
			args = append(args, "--append", "--groups", strings.Join(u.args.Groups, ","))
		}
	}
	if len(args) == 0 {
		return nil // 已收敛——这正是「连续 Apply 两次第二次无副作用」的来源
	}
	return mustRun(ctx, u.env.runner(), "收敛用户", "usermod",
		append(args, u.args.Name)...)
}

// Remove 是 no-op。
//
// 系统上可能有大量文件以该 uid 为属主，删掉用户会让它们变成孤儿 uid；
// 而下次某个新用户复用该 uid 时会**意外获得这些文件的所有权**。
// 这是真实的安全事故模式。删除由 `component remove --purge-user` 显式触发。
func (u *User) Remove(context.Context) error { return nil }

// ── group ───────────────────────────────────────────────────────────────

type groupArgs struct {
	Name   string `json:"name"`
	GID    *int   `json:"gid,omitempty"`
	System bool   `json:"system,omitempty"`
}

// Group 保证一个系统组存在。
type Group struct {
	base
	env  *Env
	args groupArgs
}

func newGroup(env *Env, r spec.Resource) (Resource, error) {
	var a groupArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}
	if a.Name == "" {
		return nil, badArg(r, "缺少 name")
	}
	return &Group{base: base{id: r.ID, typ: r.Type}, env: env, args: a}, nil
}

// Read 查 NSS。
func (g *Group) Read(ctx context.Context) (Observed, error) {
	ent, err := getent(ctx, g.env.runner(), "group", g.args.Name)
	if err != nil {
		return unknown("查询组 %s: %v", g.args.Name, err), nil
	}
	if ent == nil {
		return Observed{State: StateAbsent}, nil
	}
	if len(ent) < 3 {
		return unknown("getent group %s 的输出无法解析", g.args.Name), nil
	}
	return present(map[string]any{"gid": ent[2]}), nil
}

// Diff 比较 gid。
func (g *Group) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}
	if g.args.GID != nil {
		b.scalar("gid", strconv.Itoa(*g.args.GID), o.Field("gid"))
	}
	return b.changes
}

// Apply 建组。
func (g *Group) Apply(ctx context.Context) error {
	obs, err := g.Read(ctx)
	if err != nil {
		return err
	}
	switch obs.State {
	case StateUnknown:
		return Transient("创建组", errUnknownIdentity(g.args.Name, obs.Reason))
	case StatePresent:
		// 与 uid 同理：改 gid 会让已有文件变成孤儿属组
		if g.args.GID != nil && strconv.Itoa(*g.args.GID) != obs.Field("gid") {
			return Permanentf("收敛组",
				"组 %s 已存在且 gid 为 %s，声明要求 %d\n"+
					"  改 gid 会让已有文件变成孤儿属组，不自动执行",
				g.args.Name, obs.Field("gid"), *g.args.GID)
		}
		return nil
	}

	args := []string{}
	if g.args.System {
		args = append(args, "--system")
	}
	if g.args.GID != nil {
		args = append(args, "--gid", strconv.Itoa(*g.args.GID))
	}
	return mustRun(ctx, g.env.runner(), "创建组", "groupadd",
		append(args, g.args.Name)...)
}

// Remove 是 no-op，理由同 User.Remove。
func (g *Group) Remove(context.Context) error { return nil }

// ── 辅助 ────────────────────────────────────────────────────────────────

// supplementaryGroups 读一个用户的全部所属组。
func supplementaryGroups(ctx context.Context, r Runner, name string) ([]string, error) {
	res, err := r.Run(ctx, "id", "-nG", name)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, Transient("查询附加组", errCmd("id -nG", res))
	}
	out := strings.Fields(res.Stdout)
	sort.Strings(out)
	return out, nil
}

func sameSet(a, b []string) bool {
	if len(a) == 0 {
		return true
	}
	set := make(map[string]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	for _, x := range a {
		if !set[x] {
			return false
		}
	}
	return true
}

func errUnknownIdentity(name, reason string) error {
	return &plainErr{"无法确定 " + name + " 是否已存在（" + reason + "），本轮跳过"}
}

func errCmd(cmd string, res CmdResult) error {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	return &plainErr{cmd + " 退出码 " + strconv.Itoa(res.ExitCode) + ": " + msg}
}

type plainErr struct{ s string }

func (e *plainErr) Error() string { return e.s }
