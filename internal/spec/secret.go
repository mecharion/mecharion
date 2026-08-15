package spec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SecretPrefix 是密钥哨兵串的前缀。
//
// 完整形态为 `@@m7n:secret:<id>@@`。id 是随机的，实际内容撞上它的概率
// 可以忽略——但**不依赖概率**：mechd 渲染时检查源内容是否含此前缀，
// 含则直接报错；mechlet 替换后若仍有残留同样报错。与
// GenerationPlaceholder 完全一致的处理（16-secrets §4）。
const SecretPrefix = "@@m7n:secret:"

// SecretToken 返回某个密钥的哨兵串。
func SecretToken(id string) string { return SecretPrefix + id + "@@" }

// secretTokenRe 用于找出残留的哨兵串。
var secretTokenRe = regexp.MustCompile(regexp.QuoteMeta(SecretPrefix) + `([A-Za-z0-9_-]+)@@`)

// SecretRef 是规格中一条密钥引用。
//
// **只有 id 与版本号，没有值**。值随 gRPC 消息单独下发，不落盘。
type SecretRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	// Param 仅供诊断：出问题时能一眼看出是哪个参数，而不必反查 id。
	Param string `json:"param,omitempty"`
}

// WithSecrets 返回一份附带了密钥明文的副本。
//
// 明文存在**未导出字段**里，因此 encoding/json 根本看不见它。这不是约定
// 而是结构上的保证：它不可能进 digest（ComputeDigest 走 json.Marshal）、
// 不可能进归档、不可能被某个新写的日志语句顺手打出来。
//
// 键是**参数名**而不是密钥 id：hook 要的是
// `MECHARION_PARAM_FILE_ADMIN_PASSWORD`，id 对它没有意义。
func (s *ResolvedSpec) WithSecrets(byParam map[string]string) *ResolvedSpec {
	clone := *s
	clone.secrets = byParam
	return &clone
}

// SecretValue 取某个参数的明文。
func (s *ResolvedSpec) SecretValue(param string) (string, bool) {
	v, ok := s.secrets[param]
	return v, ok
}

// SecretParams 返回全部已附带的密钥明文，参数名 → 值。
func (s *ResolvedSpec) SecretParams() map[string]string { return s.secrets }

// secretsByParam 把 id → 明文 换算成 参数名 → 明文。
func secretsByParam(refs []SecretRef, byID map[string]string) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		v, ok := byID[r.ID]
		if !ok {
			continue
		}
		name := r.Param
		if name == "" {
			name = r.ID
		}
		out[name] = v
	}
	return out
}

// ResolveSecrets 返回把哨兵串替换为明文后的副本。
//
// 由 mechlet 在**写盘的最后一刻**调用。实现方式与 ResolveGeneration 相同
// （序列化 → 字面量替换 → 反序列化），理由也相同：哨兵串可能出现在任何
// 字符串字段，逐字段处理既冗长又容易漏。
//
// 缺失任何一个引用都报错，绝不把哨兵串当空值写进配置——那会得到一个
// 「配置看起来正常、应用却认证失败」的现场，是最难查的一类故障。
func ResolveSecrets(s *ResolvedSpec, values map[string]string) (*ResolvedSpec, error) {
	if len(s.SecretRefs) == 0 {
		clone := *s
		return &clone, nil
	}

	var missing []string
	for _, ref := range s.SecretRefs {
		if _, ok := values[ref.ID]; !ok {
			missing = append(missing, describeRef(ref))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"spec references %d secret(s), but the dispatched values are missing: %s\n"+
				"  → secrets are dispatched with the spec and never written to disk, "+
				"and get resent in full on reconnect; if this keeps happening, check "+
				"whether mechd's master key is readable",
			len(s.SecretRefs), strings.Join(missing, ", "))
	}

	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("serializing spec: %w", err)
	}
	text := string(b)
	for _, ref := range s.SecretRefs {
		esc, err := json.Marshal(values[ref.ID])
		if err != nil {
			return nil, err
		}
		text = strings.ReplaceAll(text, SecretToken(ref.ID), string(esc[1:len(esc)-1]))
	}

	if left := secretTokenRe.FindString(text); left != "" {
		return nil, fmt.Errorf(
			"secret sentinel %s is still present after substitution — this is a mechd bug, "+
				"do not work around it manually: writing the sentinel into the config just "+
				"pushes the auth failure to runtime", left)
	}

	var out ResolvedSpec
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("parsing spec after secret substitution: %w", err)
	}
	// hook 也要拿到这些值（走 0600 临时文件，不进环境变量），
	// 而它认的是参数名而非密钥 id
	out.secrets = secretsByParam(s.SecretRefs, values)
	return &out, nil
}

func describeRef(r SecretRef) string {
	if r.Param != "" {
		return fmt.Sprintf("%s(%s)", r.Param, r.ID)
	}
	return r.ID
}

// HasSecretToken 报告规格中是否还有哨兵串。物化前应当为 false。
func HasSecretToken(s *ResolvedSpec) bool {
	b, err := json.Marshal(s)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), SecretPrefix)
}
