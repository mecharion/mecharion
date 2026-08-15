package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ComputeDigest 计算规格的规范化 sha256——generation 的身份。
//
// 规范化规则：
//
//	① 序列化为 JSON，map 键按字典序（encoding/json 已保证），
//	   结构体字段按声明顺序（稳定）
//	② 排除 Digest 字段自身
//	③ 敏感值本就不在规格里（走 SecretRefs），这里再显式清一遍兜底
//
// **SecretRefs 参与 digest**，这与早期设计相反。当时写的是「密钥轮换不应
// 触发 generation 切换」，那是错的：口令一旦渲染进配置文件（而这无法避免），
// 文件内容就变了。此时若 digest 不变——
//
//	不产生新 generation → 资源层检出差异但 generation 没换 ⇒ 按漂移处理
//	⇒ 默认 driftPolicy: report ⇒ 只上报不改 ⇒ **轮换永远发不出去**
//
// 「轮换不该产生新 generation」这个直觉把「值没进 spec」和「什么都没发生」
// 混为一谈了。配置文件确实变了，重启是应当的（16-secrets §5）。
//
// 由此得到三条行为：
//   - 同一份规格重复下发 → digest 相同 → 复用现有 generation，不产生 churn
//   - 新 digest → 分配新 generation
//   - 历史 digest（回滚）→ 命中已保留的目录，**直接切软链，秒级完成**
//
// **RunState 不参与 digest**，与 SecretRefs 恰好相反，理由也恰好相反：
//
// digest 是 generation 的身份，而 generation 是**物化单位**——它回答的是
// 「盘上那份东西是什么」。停不停一个服务不改变盘上任何一个字节。
//
// 若让它参与，一次 `component stop` 会分配一个全新的 generation 目录、
// 解压同样的载荷、切一次软链，然后什么也不启动；`component start` 再来
// 一遍。回滚历史里于是堆满只差运行态的 generation，而 `retainGenerations`
// 会把真正有用的旧版本挤出去——**一次停机操作把回滚能力吃掉了**。
//
// **Suppressions 也不参与**，理由同上且更尖锐：`ack-drift` 是**事故当中**
// 敲的命令——运维刚手工改了一个值，想让工具别报警。如果这条命令顺带切一次
// generation，它会把那个人刚改好的东西**当场覆盖掉并重启服务**。
// **Resource.DriftPolicy 同样不参与**，理由与 RunState 相同：它是「机器变了
// 该怎么办」，不是「盘上应该是什么」。
//
// 后果比 RunState 更要紧。站点侧放松策略（reconcile → report）多半发生在
// **事故当中**——运维正想临时改个值而不被工具改回去。若它进 digest，
// 这条命令会切一次 generation、把服务重启一遍。§4.3 花了整节说
// 「自动改回不得顺带重启」，而这会从另一条路径犯同一个错。
func ComputeDigest(s *ResolvedSpec) (string, error) {
	clone := *s
	clone.Digest = ""        // ② 排除自身
	clone.RunState = ""      // 运行态不是物化内容，见上
	clone.Removal = nil      // 「拆的时候怎么拆」同理，见上
	clone.Suppressions = nil // 抑制同理，见下

	if len(clone.Resources) > 0 {
		res := make([]Resource, len(clone.Resources))
		copy(res, clone.Resources)
		for i := range res {
			res[i].DriftPolicy = ""
		}
		clone.Resources = res
	}

	// ③ 敏感值不参与摘要：Sensitive 参数的 Value 本就为空，
	// 这里再显式清一遍，防止调用方误填。
	if len(clone.Params) > 0 {
		params := make(map[string]ParamValue, len(clone.Params))
		for k, v := range clone.Params {
			if v.Sensitive {
				v.Value = nil
			}
			params[k] = v
		}
		clone.Params = params
	}

	b, err := json.Marshal(&clone)
	if err != nil {
		return "", fmt.Errorf("serializing spec: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Seal 计算并写入 Digest。
func Seal(s *ResolvedSpec) error {
	d, err := ComputeDigest(s)
	if err != nil {
		return err
	}
	s.Digest = d
	return nil
}

// VerifyDigest 校验规格的 Digest 与内容一致。
func VerifyDigest(s *ResolvedSpec) error {
	if s.Digest == "" {
		return fmt.Errorf("spec is missing a digest")
	}
	want, err := ComputeDigest(s)
	if err != nil {
		return err
	}
	if want != s.Digest {
		return fmt.Errorf("digest mismatch: declared %s, actual %s", short(s.Digest), short(want))
	}
	return nil
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12] + "…"
	}
	return d
}

// ── 解析与校验 ──────────────────────────────────────────────────────────

// Marshal 把规格序列化为下发用的 JSON。
//
// 顺带校验 digest 自洽：一份 digest 对不上的规格发出去，接收方要么拒收、
// 要么按错误的身份复用 generation。在发送端拦住，错误信息里还有上下文。
func Marshal(s *ResolvedSpec) ([]byte, error) {
	if err := VerifyDigest(s); err != nil {
		return nil, fmt.Errorf("%s/%s@%s: %w", s.Component, s.Role, s.Node.Name, err)
	}
	return json.Marshal(s)
}

// Parse 从 JSON 解析规格并做基本校验。
func Parse(data []byte) (*ResolvedSpec, error) {
	var s ResolvedSpec
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // 未知字段报错而非忽略——静默忽略会让行为与下发方预期不符
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parsing spec: %w", err)
	}
	if err := Validate(&s); err != nil {
		return nil, err
	}
	s.Reconcile = s.Reconcile.WithDefaults()
	return &s, nil
}

// Load 从文件读取规格。
func Load(path string) (*ResolvedSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	s, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Validate 做结构性校验。
func Validate(s *ResolvedSpec) error {
	if s.SchemaVersion == 0 {
		return fmt.Errorf("missing schemaVersion")
	}
	if s.SchemaVersion > SchemaVersion {
		return fmt.Errorf(
			"spec's schemaVersion=%d is newer than this mechlet's supported %d\n"+
				"  the control plane stays backward-compatible with agents — upgrade mechlet first",
			s.SchemaVersion, SchemaVersion)
	}
	if s.Component == "" {
		return fmt.Errorf("missing component")
	}
	if s.Role == "" {
		return fmt.Errorf("missing role")
	}
	if s.Node.Name == "" {
		return fmt.Errorf("missing node.name")
	}
	if s.ConfigGroup == "" {
		s.ConfigGroup = "default"
	}

	for name, p := range s.Paths {
		if len(p.Values) == 0 {
			return fmt.Errorf("paths.%s has no values", name)
		}
		if p.Kind == "multi" && p.Layout == "inline" {
			return fmt.Errorf("paths.%s: kind=multi cannot be combined with layout=inline", name)
		}
	}

	seen := map[string]bool{}
	for i, r := range s.Resources {
		if r.Type == "" {
			return fmt.Errorf("resources[%d] missing type", i)
		}
		if r.ID == "" {
			return fmt.Errorf("resources[%d] (%s) missing id", i, r.Type)
		}
		if seen[r.ID] {
			return fmt.Errorf("resources[%d]: duplicate id %q", i, r.ID)
		}
		seen[r.ID] = true
	}

	if w := s.Workload; w != nil {
		switch w.Runtime {
		case "systemd":
			if w.Systemd == nil {
				return fmt.Errorf("workload.runtime=systemd but missing the systemd section")
			}
			if strings.TrimSpace(w.Systemd.Exec) == "" {
				return fmt.Errorf("workload.systemd.exec is empty")
			}
		case "docker", "compose":
			// 故意不校验：Docker/Compose 是 json.RawMessage（内部结构对
			// 这一层不透明），具体形状由各自的 Runtime 解析时校验，不在
			// 这里重复解析一遍。
		case "":
			return fmt.Errorf("workload missing runtime")
		default:
			return fmt.Errorf("unknown runtime %q", w.Runtime)
		}
	}

	if h := s.Health; h != nil {
		n := 0
		for _, ok := range []bool{h.HTTP != nil, h.TCP != nil, h.Exec != nil} {
			if ok {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("health must declare exactly one probe, got %d", n)
		}
	}
	return nil
}

// ── generation 占位符替换 ───────────────────────────────────────────────

// ResolveGeneration 返回把 GenerationPlaceholder 替换为 genDir 后的副本。
//
// 实现方式是「序列化 → 字面量替换 → 反序列化」。这样做而非逐字段遍历，
// 是因为占位符可能出现在任何字符串字段（paths、resources 的 Args、
// workload 的 exec……），逐字段处理既冗长又容易漏。
//
// 注意这是**字面量替换，不是模板渲染**——不引入第二个渲染阶段。
func ResolveGeneration(s *ResolvedSpec, genDir string) (*ResolvedSpec, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("serializing spec: %w", err)
	}
	if !strings.Contains(string(b), GenerationPlaceholder) {
		clone := *s
		return &clone, nil
	}

	// genDir 会被嵌进 JSON 字符串，必须先转义
	escaped, err := json.Marshal(genDir)
	if err != nil {
		return nil, err
	}
	// 去掉 json.Marshal 加的两端引号
	repl := string(escaped[1 : len(escaped)-1])

	replaced := strings.ReplaceAll(string(b), GenerationPlaceholder, repl)

	var out ResolvedSpec
	if err := json.Unmarshal([]byte(replaced), &out); err != nil {
		return nil, fmt.Errorf("parsing after replacing generation placeholder: %w", err)
	}
	// 序列化往返会丢掉未导出字段。密钥明文正是靠「json 看不见它」来保证
	// 不外泄的，代价就是每一处 marshal→unmarshal 都必须显式带上它。
	out.secrets = s.secrets
	return &out, nil
}

// HasUnresolvedPlaceholder 报告规格中是否仍有未替换的占位符。
// 物化前应当为 false ——否则会把字面量 "{{ .Paths.Generation }}" 写进文件系统。
func HasUnresolvedPlaceholder(s *ResolvedSpec) bool {
	b, err := json.Marshal(s)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), GenerationPlaceholder)
}
