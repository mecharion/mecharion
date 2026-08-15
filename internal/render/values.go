package render

import (
	"encoding/json"
	"time"

	"github.com/mecharion/mecharion/internal/pack"
)

// SizeValue 是 `type: size` 参数在模板中的形态。
//
// `{{ .Params.max_body_size }}` 得到 `4MB`，`{{ .Params.max_body_size.Bytes }}`
// 得到 `4000000`——spec §7.2 承诺了后者，而应用要哪一种由它自己决定：
// nginx 的 `client_max_body_size` 要带单位，Go 应用的 YAML 字段要纯数字。
//
// 没有这层的话，Pack 作者只能在模板里手写单位换算，那既啰嗦又必然出错。
type SizeValue struct {
	Literal string
	Bytes   int64
}

// String 让 `{{ .Params.x }}` 渲染成原本的字面量。
func (v SizeValue) String() string { return v.Literal }

// MarshalJSON 让规格里存的是字面量而非结构体。
//
// 规格是**线格式**，读它的 mechlet 与诊断工具不需要这层富类型；
// 存成对象还会让 digest 依赖一个纯属渲染期便利的实现细节。
func (v SizeValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.Literal) }

// DurationValue 是 `type: duration` 参数在模板中的形态。
//
// nginx 的 `keepalive_timeout` 要秒数、PostgreSQL 的
// `log_min_duration_statement` 要毫秒数、ZooKeeper 的 `tickTime` 也要毫秒——
// 三种单位都得能取到，否则 Pack 作者只能在模板里手算。
type DurationValue struct {
	Literal      string
	Seconds      int64
	Milliseconds int64
	Nanoseconds  int64
}

// String 让 `{{ .Params.x }}` 渲染成原本的字面量。
func (v DurationValue) String() string { return v.Literal }

// MarshalJSON 同 SizeValue：规格里只留字面量。
func (v DurationValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.Literal) }

// enrich 把 size / duration 参数换成带访问器的富类型。
//
// 幂等：已经是富类型的原样返回。管线里有三处会写入参数（静态链、
// defaultFrom/generate、from），每处之后都要调一次，而不是只在最后调——
// 后续阶段的表达式也会读 `.Params`。
func enrich(decls map[string]pack.Param, params map[string]any) {
	for name, raw := range params {
		d, ok := decls[name]
		if !ok {
			continue
		}
		switch d.Type {
		case pack.TypeSize:
			s, ok := raw.(string)
			if !ok {
				continue // 已经是 SizeValue，或类型对不上（校验阶段会报）
			}
			n, err := pack.ParseSize(s)
			if err != nil {
				continue // 非法值由校验阶段负责报错，这里不越俎代庖
			}
			params[name] = SizeValue{Literal: s, Bytes: n}

		case pack.TypeDuration:
			s, ok := raw.(string)
			if !ok {
				continue
			}
			dur, err := time.ParseDuration(s)
			if err != nil {
				continue
			}
			params[name] = DurationValue{
				Literal:      s,
				Seconds:      int64(dur / time.Second),
				Milliseconds: dur.Milliseconds(),
				Nanoseconds:  dur.Nanoseconds(),
			}
		}
	}
}

// plainValue 剥掉富类型，用于落进规格与做值比较。
func plainValue(v any) any {
	switch x := v.(type) {
	case SizeValue:
		return x.Literal
	case DurationValue:
		return x.Literal
	}
	return v
}
