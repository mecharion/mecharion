package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
)

// EnvPrefix 是全部注入变量的前缀。
const EnvPrefix = "MECHARION_"

// buildEnv 构造 hook 的**完整**环境（spec §16.5）。
//
// 是替换而非追加：继承一份开发机上恰好存在的变量，会让「在我这儿能跑」
// 变成常态。PATH 显式给一个保守值——脚本要调 psql 之类的东西，
// 而它们的位置来自 MECHARION_PATHS_*，不该指望 PATH。
func (e *Executor) buildEnv(s *spec.ResolvedSpec, secretFiles map[string]string) []string {
	kv := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME": "/root",

		EnvPrefix + "COMPONENT":    s.Component,
		EnvPrefix + "ROLE":         s.Role,
		EnvPrefix + "CONFIG_GROUP": s.ConfigGroup,
		EnvPrefix + "PROFILE":      s.Profile,
		EnvPrefix + "NODE":         s.Node.Name,
		EnvPrefix + "ORDINAL":      strconv.Itoa(s.Ordinal),
		EnvPrefix + "PACK":         s.Pack.Name,
		EnvPrefix + "PACK_VERSION": s.Pack.Version,
		EnvPrefix + "SITE":         s.Site.Name,
	}
	if e.GenerationDir != "" {
		kv[EnvPrefix+"GENERATION"] = e.GenerationDir
	}

	// 路径：多值用 ':' 连接，与 PATH 一致，shell 里 IFS=: 就能拆
	for name, pv := range s.Paths {
		kv[EnvPrefix+"PATHS_"+envName(name)] = strings.Join(pv.Values, ":")
	}

	// 参数：敏感的**不进环境变量**
	for name, pval := range s.Params {
		if pval.Sensitive {
			continue
		}
		kv[EnvPrefix+"PARAM_"+envName(name)] = fmt.Sprintf("%v", pval.Value)
	}
	// 敏感参数只给一个指向 0600 文件的路径
	for name, path := range secretFiles {
		kv[EnvPrefix+"PARAM_FILE_"+envName(name)] = path
	}

	out := make([]string, 0, len(kv))
	for _, k := range sortedKeys(kv) {
		out = append(out, k+"="+kv[k])
	}
	return out
}

// envName 把参数 / 路径名变成环境变量名的那一段。
//
// 规则：camelCase 拆成下划线，再整体大写。`dataDirs` → `DATA_DIRS`，
// `admin_password` → `ADMIN_PASSWORD`。
//
// 拆 camelCase 而不是简单大写，是因为路径名按约定是 camelCase 而参数名
// 是 snake_case；不拆的话 `dataDirs` 会变成 `DATADIRS`，读的人得先知道
// 原名才拼得出来。
func envName(s string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevLower = false
		case (r >= 'a' && r <= 'z'):
			b.WriteRune(r - 32)
			prevLower = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = false
		default:
			b.WriteByte('_')
			prevLower = false
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeSecretFiles 把敏感参数落到 0600 临时文件，返回 参数名 → 路径。
//
// **敏感参数不进环境变量**：环境变量会出现在 `/proc/<pid>/environ` 与
// 崩溃转储里，且被子进程继承。把口令交给一个任意脚本时，这是最容易漏掉
// 的泄漏面（16-secrets §6）。
//
// 目录 0700、文件 0600，落在 tmpfs 上，调用方在 hook 结束后整个删掉。
func (e *Executor) writeSecretFiles(s *spec.ResolvedSpec) (string, map[string]string, error) {
	values := s.SecretParams()
	if len(values) == 0 {
		return "", nil, nil
	}

	root := e.RunDir
	if root == "" {
		root = DefaultRunDir
	}
	if err := makeTraversable(root); err != nil {
		return "", nil, faults.Wrap(faults.Transient, "准备 hook 密钥目录", err)
	}
	dir, err := os.MkdirTemp(root, "h-")
	if err != nil {
		return "", nil, faults.Wrap(faults.Transient, "准备 hook 密钥目录", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", nil, faults.Wrap(faults.Transient, "准备 hook 密钥目录", err)
	}

	files := make(map[string]string, len(values))
	for _, name := range sortedKeys(values) {
		p := filepath.Join(dir, name)
		// 先建成 0600 再写：先写后 chmod 会留下一个短暂的可读窗口
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			os.RemoveAll(dir)
			return "", nil, faults.Wrap(faults.Transient, "写入 hook 密钥文件", err)
		}
		_, werr := f.WriteString(values[name])
		cerr := f.Close()
		if werr != nil || cerr != nil {
			os.RemoveAll(dir)
			return "", nil, faults.Wrap(faults.Transient, "写入 hook 密钥文件",
				fmt.Errorf("%v %v", werr, cerr))
		}
		files[name] = p
	}
	return dir, files, nil
}

// makeTraversable 建出密钥目录，并保证**整条路径都能被穿过**。
//
// 0711 是「可穿过但不可列举」：以非 root 身份运行的 hook 需要 +x 才能
// 走到那一层，而 +r 会把「这台机器上有哪些组件在跑什么」公开出去。
// 真正的保护在最下一层——0700、属主是该 hook 的身份、目录名随机。
//
// 关键在于**逐级处理，而不只是最后一级**：`MkdirAll` 对已存在的目录
// 不改权限，因此一个早先被建成 0700 的 `/run/mecharion` 会让下面全部
// 白做——现象是「文件权限、属主都对，就是读不到」，很费时间。
func makeTraversable(dir string) error {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o711); err != nil {
		return err
	}

	// 再自**父目录**往上补 +x，遇到第一个已经可穿过的层就停。
	//
	// 必须从父目录起：叶子刚被建成 0711，从它开始判断会当场满足
	// 「已经可穿过」而直接退出——上面那个 0700 的 /run/mecharion
	// 就永远轮不到检查。症状是文件的权限与属主都对、却仍然读不到，
	// 排查时极易盯着叶子看。
	//
	// 「第一个可穿过的层就停」同时回答了「哪些是我们的目录」：系统目录
	// （/run、/var）本来就是 0755，一碰就停，因此绝不会被放宽；而 mechlet
	// 自己早先建成 0700 的那些层会被修正——「上一次进程建的」这件事
	// 跨进程无从判断，只能靠这条规则。
	for p := filepath.Dir(filepath.Clean(dir)); ; p = filepath.Dir(p) {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0o001 != 0 {
			break
		}
		if err := os.Chmod(p, st.Mode().Perm()|0o001); err != nil {
			return err
		}
		if parent := filepath.Dir(p); parent == p {
			break
		}
	}
	return nil
}

// ChownSecrets 把密钥文件的属主改成 hook 的运行身份。
//
// 以非 root 身份跑的 hook 读不了 root 拥有的 0600 文件。放宽权限位是错的
// 解法——那等于让同机任何用户都读得到。
func ChownSecrets(dir string, uid, gid int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	for _, ent := range entries {
		if err := os.Chown(filepath.Join(dir, ent.Name()), uid, gid); err != nil {
			return err
		}
	}
	return nil
}
