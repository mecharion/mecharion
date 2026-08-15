package mechd

import (
	"context"
	"errors"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/password"
	"github.com/mecharion/mecharion/internal/store"
)

// AdminUser 是**唯一**的账号名。
//
// 不支持增加账户（[ADR-0037](../../docs/adr/0037-login-is-full-privilege.md)）：
// 没有角色划分的多用户在权限隔离上等于零，它能提供的问责与凭据生命周期，
// 要等角色做出来之后才成立。初始化页与登录页都把它显示成不可编辑的固定值。
const AdminUser = "admin"

// MinPasswordLen 是口令长度下限。
//
// **只管长度，不管「必须含大写数字符号」**：那类规则被反复证明会把人推向
// `Passw0rd!` 这种可预测的模式，而长度才是真正拉高爆破成本的东西。
const MinPasswordLen = 12

// ErrAlreadyInitialized 表示管理员已经设过了。
//
// 初始化是**一次性**的：设过之后端点永久拒绝，不给第二次机会。
var ErrAlreadyInitialized = errors.New("admin has already been initialized")

// AdminView 是管理员账号对外的样子。
//
// **没有 PasswordHash**。哈希不该出现在任何响应里——即使它不可逆，
// 泄漏出去就等于把离线爆破的门打开了。
type AdminView struct {
	Name string `json:"name"`
	// Initialized 为 false 表示这台机器还没人设过口令。
	Initialized bool   `json:"initialized"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

// Initialized 报告管理员是否已经设过口令。
//
// 未初始化时**任何能访问 UI 的人都能完成初始化**（ADR-0037 记在案的代价）。
// 因此这个状态要在几个地方都看得见：启动日志、`mechctl user show`、
// 以及初始化页本身。
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	n, err := s.Repos.Users().Count(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// InitializeAdmin 设定管理员口令，**只能成功一次**。
//
// from 是发起方地址，进审计——事后要回答得了「是谁初始化的」。
func (s *Service) InitializeAdmin(
	ctx context.Context, plain, from string,
) (*AdminView, error) {
	if err := checkPassword(plain); err != nil {
		return nil, err
	}
	// **先查再建**：并发两个请求时靠 name 上的 UNIQUE 兜底，
	// 这里只是给出一句像话的错误
	done, err := s.Initialized(ctx)
	if err != nil {
		return nil, err
	}
	if done {
		return nil, ErrAlreadyInitialized
	}

	hash, err := password.Hash(plain)
	if err != nil {
		return nil, err
	}
	u, err := s.Repos.Users().Create(ctx, AdminUser, hash, s.now())
	if err != nil {
		return nil, err
	}
	// 审计里记来源，不记口令也不记哈希
	s.audit(ctx, "bootstrap", "admin-init", AdminUser, nil, "from "+from)
	s.log().Warn("admin initialized", "from", from)

	v := adminView(u, true)
	return &v, nil
}

// AdminStatus 返回管理员账号的状态。
func (s *Service) AdminStatus(ctx context.Context) (*AdminView, error) {
	u, err := s.Repos.Users().GetByName(ctx, AdminUser)
	if errors.Is(err, store.ErrNotFound) {
		return &AdminView{Name: AdminUser, Initialized: false}, nil
	}
	if err != nil {
		return nil, err
	}
	v := adminView(u, true)
	return &v, nil
}

// SetAdminPassword 重设管理员口令。
//
// 这是**服务器侧的补救通道**：口令忘了、或者要把机器交给下一任。
// 它要 admin token，因此只有能登上那台机器的人做得了。
func (s *Service) SetAdminPassword(ctx context.Context, plain, actor string) error {
	if err := checkPassword(plain); err != nil {
		return err
	}
	if _, err := s.Repos.Users().GetByName(ctx, AdminUser); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return faults.Permanentf("", "admin has not been initialized yet -- open the UI in a browser first to complete initialization")
		}
		return err
	}
	hash, err := password.Hash(plain)
	if err != nil {
		return err
	}
	if err := s.Repos.Users().SetPassword(ctx, AdminUser, hash, s.now()); err != nil {
		return err
	}
	// **改完口令把已有会话全清掉。**
	//
	// 不清的话，一个已经被偷走的会话在改完口令之后仍然有效——而改口令的
	// 动机通常正是「怀疑被偷了」。
	if err := s.EndAllSessions(ctx, AdminUser); err != nil {
		s.log().Warn("failed to clear sessions, old sessions may still be valid", "err", err)
	}
	s.audit(ctx, actor, "admin-passwd", AdminUser, nil, "changed")
	return nil
}

// ResetAdmin 抹掉管理员，**重新打开初始化窗口**。
//
// 它是「把机器交还给下一任」的出口。危险之处要由调用方说清楚：
// 抹掉之后，任何能访问 UI 的人都能重新完成初始化。
func (s *Service) ResetAdmin(ctx context.Context, actor string) error {
	if _, err := s.Repos.Users().GetByName(ctx, AdminUser); err != nil {
		return err
	}
	if err := s.Repos.Users().Delete(ctx, AdminUser); err != nil {
		return err
	}
	if err := s.EndAllSessions(ctx, AdminUser); err != nil {
		s.log().Warn("failed to clear sessions, old sessions may still be valid", "err", err)
	}
	s.audit(ctx, actor, "admin-reset", AdminUser, nil, "reset")
	s.log().Warn("admin was wiped, the initialization window is open again")
	return nil
}

// Authenticate 校验管理员口令。
//
// **用户名也要对**：虽然只有一个账号，但接口收到什么就验什么——
// 一个「用户名随便填都行」的实现会在将来加账号时变成一个洞。
//
// **不区分「用户名不对」与「口令错」**：两种情况都走完一次哈希再回同一句话，
// 否则「这个用户存在吗」会变成一个计时问题。
func (s *Service) Authenticate(ctx context.Context, name, plain string) error {
	u, err := s.Repos.Users().GetByName(ctx, AdminUser)
	if err != nil || name != AdminUser {
		// 名字不对或还没初始化，都烧掉一次哈希的时间
		_ = password.Verify("", plain)
		return password.ErrMismatch
	}
	if err := password.Verify(u.PasswordHash, plain); err != nil {
		return password.ErrMismatch
	}
	// 参数调强过就顺手重算——**这是唯一能拿到明文的时刻**
	if password.NeedsRehash(u.PasswordHash) {
		if hash, err := password.Hash(plain); err == nil {
			_ = s.Repos.Users().SetPassword(ctx, AdminUser, hash, s.now())
		}
	}
	return nil
}

func adminView(u store.User, initialized bool) AdminView {
	return AdminView{
		Name: u.Name, Initialized: initialized,
		CreatedAt: store.FormatTime(u.CreatedAt),
		UpdatedAt: store.FormatTime(u.UpdatedAt),
	}
}

func checkPassword(plain string) error {
	if len([]rune(plain)) < MinPasswordLen {
		return faults.Permanentf("", "password must be at least %d characters (length matters more than mixing case and symbols)",
			MinPasswordLen)
	}
	return nil
}
