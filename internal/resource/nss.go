package resource

import (
	"os/user"
	"strings"
)

// getentFallback 在没有 getent 命令的机器（Windows / macOS 开发机）上，
// 用 os/user 拼出与 getent 相同格式的字段切片。
//
// 它**只在 getent 不存在时**使用。生产环境是 Linux，永远走 getent——
// CGO_ENABLED=0 下的 os/user 只读 /etc/passwd，看不见 LDAP/SSSD，
// 不能作为正式路径。
// getent 的 key 既可以是名字也可以是 id，而 os/user 用两组不同的函数
// 处理它们。**由调用方告知方向**，不靠「看起来像数字」来猜——Windows 上
// 的 uid 是 SID 字符串，猜法在那里直接失效。
func getentFallback(db, key string, numeric bool) ([]string, error) {
	switch db {
	case "passwd":
		lookup := user.Lookup
		if numeric {
			lookup = user.LookupId
		}
		u, err := lookup(key)
		if err != nil {
			if isUnknownIdentity(err) {
				return nil, nil
			}
			return nil, Transient("查询 passwd", err)
		}
		// name:x:uid:gid:gecos:home:shell —— 本地读不到 shell，留空
		return []string{u.Username, "x", u.Uid, u.Gid, u.Name, u.HomeDir, ""}, nil

	case "group":
		lookup := user.LookupGroup
		if numeric {
			lookup = user.LookupGroupId
		}
		g, err := lookup(key)
		if err != nil {
			if isUnknownIdentity(err) {
				return nil, nil
			}
			return nil, Transient("查询 group", err)
		}
		// name:x:gid:members —— 本地读不到成员列表，留空
		return []string{g.Name, "x", g.Gid, ""}, nil

	default:
		return nil, Permanentf("查询 "+db, "不支持的 NSS 数据库 %q", db)
	}
}

// isUnknownIdentity 区分「确实不存在」与「查询失败」。
// os/user 用两个具体错误类型表达前者。
func isUnknownIdentity(err error) bool {
	switch err.(type) {
	case user.UnknownUserError, user.UnknownGroupError,
		user.UnknownUserIdError, user.UnknownGroupIdError:
		return true
	}
	// Windows 上 os/user 返回未导出的错误类型，只能看文本
	return strings.Contains(err.Error(), "unknown user") ||
		strings.Contains(err.Error(), "unknown group") ||
		strings.Contains(err.Error(), "No mapping between account names")
}
