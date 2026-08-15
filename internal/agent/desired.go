package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/pathutil"
	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/vault"
)

// DesiredStore 是 mechd 下发的期望状态在本机的副本。
//
// 它存在的唯一理由是**周期调和**：没有它，mechlet 只在收到推送时干活，
// 而「有人手工改了配置」永远不会被发现——那正是常驻 Agent 相对 Ansible
// 的核心价值（ADR-0001）。
//
// 落盘的规格里是密钥哨兵，明文在 vault 里单独加密存放（ADR-0033）。
// 两者分开不是洁癖：规格会进日志、进诊断包、被人 cat；密钥不该跟着走。
type DesiredStore struct {
	dir   string
	vault *vault.FileVault
}

// NewDesiredStore 打开（必要时创建）本机的期望状态目录。
func NewDesiredStore(dir string, v *vault.FileVault) (*DesiredStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("desired: 缺少目录")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("desired: 创建目录 %s: %w", dir, err)
	}
	return &DesiredStore{dir: dir, vault: v}, nil
}

// Save 落一份实例的期望状态。
//
// **密钥与规格分两处写**，且规格先写：反过来的话，一次中途失败会留下
// 「有密钥没规格」——那份密钥没有主人，而没人会想起去清它。
func (s *DesiredStore) Save(in protocol.InstanceSpec) error {
	if in.Spec == nil {
		return fmt.Errorf("desired: 规格为空")
	}
	body, err := json.MarshalIndent(in.Spec, "", "  ")
	if err != nil {
		return fmt.Errorf("desired: 序列化 %s: %w", in.Spec.InstanceKey(), err)
	}
	// 0600：规格里没有明文密钥，但它完整描述了这台机器上跑什么、
	// 配置长什么样。没有理由让它可被他人读取。
	p, err := s.pathFor(in.Spec.InstanceKey())
	if err != nil {
		return err
	}
	if err := writeAtomic(p, body); err != nil {
		return fmt.Errorf("desired: 写入 %s: %w", in.Spec.InstanceKey(), err)
	}

	if len(in.Secrets) > 0 && s.vault != nil {
		if err := s.vault.Merge(in.Secrets); err != nil {
			return err
		}
	}
	return nil
}

// Load 读回全部期望状态，并把密钥明文补上。
//
// 返回的每一项都可以直接交给调和器。**取不到密钥的实例会带着错误返回**，
// 而不是被悄悄跳过：一个「因为密钥读不出来所以一直没被调和」的实例，
// 从外部看与「一切正常」完全一样。
func (s *DesiredStore) Load() ([]protocol.InstanceSpec, error) {
	names, err := s.list()
	if err != nil {
		return nil, err
	}

	out := make([]protocol.InstanceSpec, 0, len(names))
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, fmt.Errorf("desired: 读取 %s: %w", name, err)
		}
		var sp spec.ResolvedSpec
		if err := json.Unmarshal(body, &sp); err != nil {
			return nil, fmt.Errorf("desired: 解析 %s: %w", name, err)
		}

		secrets, err := s.secretsFor(&sp)
		if err != nil {
			return nil, err
		}
		out = append(out, protocol.InstanceSpec{Spec: &sp, Secrets: secrets})
	}
	return out, nil
}

// secretsFor 从保管库取出一份规格需要的全部密钥。
func (s *DesiredStore) secretsFor(sp *spec.ResolvedSpec) (map[string]string, error) {
	if len(sp.SecretRefs) == 0 {
		return nil, nil
	}
	if s.vault == nil {
		return nil, fmt.Errorf(
			"desired: %s 引用了 %d 条密钥，但本机没有保管库",
			sp.InstanceKey(), len(sp.SecretRefs))
	}

	out := make(map[string]string, len(sp.SecretRefs))
	var missing []string
	for _, ref := range sp.SecretRefs {
		v, ok, err := s.vault.Get(ref.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, ref.Param)
			continue
		}
		out[ref.ID] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"desired: %s 缺少密钥 %s\n"+
				"  节点侧的密钥只是缓存，源头在 mechd——等一次推送即可恢复",
			sp.InstanceKey(), strings.Join(missing, ", "))
	}
	return out, nil
}

// Keep 删掉不在集合里的实例与它们的密钥。
//
// **只在全量下发之后调用。** 增量下发只带一部分实例，按它清理会把其余的
// 全删掉——那是一次静默的、看起来像「组件自己消失了」的事故。
func (s *DesiredStore) Keep(keys map[string]bool) error {
	names, err := s.list()
	if err != nil {
		return err
	}
	for _, name := range names {
		key := keyOf(name)
		if keys[key] {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("desired: 删除 %s: %w", name, err)
		}
		// 回滚点跟着实例走：留下一个没有主人的回滚点，下次同名组件
		// 被部署时会拿到一份来路不明的旧规格
		ap, err := s.appliedPath(key)
		if err != nil {
			return err
		}
		if err := os.Remove(ap); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("desired: 删除回滚点 %s: %w", key, err)
		}
	}

	if s.vault == nil {
		return nil
	}
	// 密钥按**剩下的规格实际引用的 id** 收敛，而不是按实例名——
	// 两个实例可以引用同一条密钥（同组件的多个角色）。
	specs, err := s.Load()
	if err != nil {
		// 读不出来时**不删**：宁可留着几条多余的密文，也不要因为一次
		// 解析失败把还在用的凭据清掉
		return err
	}
	ids := map[string]bool{}
	for _, in := range specs {
		for _, ref := range in.Spec.SecretRefs {
			ids[ref.ID] = true
		}
		// **回滚点引用的密钥也要留住。**
		//
		// 升级时轮换了口令：新规格引用新 id，旧规格引用旧 id。只按新规格
		// 收敛的话，旧 id 当场被清掉——而回滚要重新渲染配置文件，那时
		// 解不出明文，**回滚本身失败**。那是最不该失败的一步。
		ap, err := s.appliedPath(in.Spec.InstanceKey())
		if err != nil {
			return err
		}
		prev, perr := s.readSpec(ap)
		if perr != nil {
			return perr
		}
		if prev != nil {
			for _, ref := range prev.SecretRefs {
				ids[ref.ID] = true
			}
		}
	}
	return s.vault.Keep(ids)
}

// list 返回目录里全部规格文件名，顺序稳定。
func (s *DesiredStore) list() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("desired: 列举 %s: %w", s.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// **回滚点不是期望状态**，不能被当成一个实例读出来——
		// 否则同一个组件会被调和两遍，第二遍还是旧版本。
		if strings.HasSuffix(e.Name(), appliedSuffix) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// pathFor 把实例键映射成文件名。
//
// `spec.InstanceKey()` 已经是 `<component>__<role>`——**不含斜杠，直接可以
// 当文件名**。不要在这里再做一次转换：那会让 pathFor 与 keyOf 各自维护一份
// 映射，而 Keep 的入参来自 InstanceKey()，两边稍有出入就会把还在用的实例
// 当成孤儿删掉。
//
// 也不用 base64 或哈希：**目录要能被人直接看懂**——排障时 `ls` 一眼就知道
// 这台机器上应该跑什么（原则七 现场可诊断）。
//
// **Component 已经在 mechd 落库时被 internal/ident 校验过，但 Role 来自
// Pack 定义，只在 `mechpack lint` 时被校验过——lint 是可选步骤。** 这里
// 用 pathutil.EnsureWithin 兜底：不管 key 里混进了什么，落到磁盘上的
// 路径必须仍然在 s.dir 之内。
func (s *DesiredStore) pathFor(key string) (string, error) {
	return safeJoin(s.dir, key+".json")
}

// safeJoin 拼接并校验结果仍落在 dir 之内。
func safeJoin(dir, name string) (string, error) {
	p := filepath.Join(dir, name)
	if err := pathutil.EnsureWithin(dir, p); err != nil {
		return "", fmt.Errorf("desired: %w", err)
	}
	return p, nil
}

func keyOf(name string) string {
	return strings.TrimSuffix(name, ".json")
}

// writeAtomic 以 0600 原子写入。
func writeAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// appliedSuffix 标记「最后一次成功应用的规格」。
//
// **不叫 .prev.json（上一版）是刻意的。** 「上一个不同的规格」与
// 「最后一次成功应用的规格」在正常路径上一样，一旦出过错就分道扬镳：
//
//	v1 成功 → v2 失败 → 用户改参数成 v3 → v3 也失败
//	  上一版        = v2   ← **从来没成功过**，回滚到它等于回滚到另一个坏版本
//	  最后成功应用的 = v1   ← 这才是能落脚的地方
//
// 自动回滚要的是后者（21-upgrade-and-rollback §2.1）。
const appliedSuffix = ".applied.json"

// MarkApplied 把一份规格记为「最后一次成功应用的」。
//
// **只在调和成功之后调用。** 写在应用之前的话，一个失败的版本会立刻变成
// 回滚目标——那正是上面那个陷阱。
func (s *DesiredStore) MarkApplied(in protocol.InstanceSpec) error {
	if in.Spec == nil {
		return fmt.Errorf("desired: 规格为空")
	}
	key := in.Spec.InstanceKey()

	ap, err := s.appliedPath(key)
	if err != nil {
		return err
	}

	// 内容没变就不写：每 60 秒重写一次同样的文件，只是让 mtime 抖动，
	// 而排障时「这个回滚点是什么时候定下的」是有意义的信息。
	if cur, err := s.readSpec(ap); err == nil &&
		cur != nil && cur.Digest == in.Spec.Digest {
		return nil
	}

	body, err := json.MarshalIndent(in.Spec, "", "  ")
	if err != nil {
		return fmt.Errorf("desired: 序列化 %s: %w", key, err)
	}
	if err := writeAtomic(ap, body); err != nil {
		return fmt.Errorf("desired: 写入回滚点 %s: %w", key, err)
	}
	return nil
}

// Applied 读回某个实例的回滚点。不存在时返回 (nil, nil)。
//
// 密钥一并补上——回滚要重新渲染配置，没有明文就写不出文件。
func (s *DesiredStore) Applied(key string) (*protocol.InstanceSpec, error) {
	ap, err := s.appliedPath(key)
	if err != nil {
		return nil, err
	}
	sp, err := s.readSpec(ap)
	if err != nil || sp == nil {
		return nil, err
	}
	secrets, err := s.secretsFor(sp)
	if err != nil {
		return nil, err
	}
	return &protocol.InstanceSpec{Spec: sp, Secrets: secrets}, nil
}

// readSpec 读一份规格；文件不存在时返回 (nil, nil)。
func (s *DesiredStore) readSpec(path string) (*spec.ResolvedSpec, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("desired: 读取 %s: %w", path, err)
	}
	var sp spec.ResolvedSpec
	if err := json.Unmarshal(body, &sp); err != nil {
		return nil, fmt.Errorf("desired: 解析 %s: %w", path, err)
	}
	return &sp, nil
}

func (s *DesiredStore) appliedPath(key string) (string, error) {
	return safeJoin(s.dir, key+appliedSuffix)
}
