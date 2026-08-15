package vault

import (
	"context"
	"fmt"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/render"
)

// RenderStore 把 Vault 接到解析管线的 render.SecretStore 上。
//
// 适配器放在这一侧而不是 render 里，是为了让 render 保持无重依赖：
// 它是那条「离线可复现」的纯函数管线，不该因为要存密钥就拖进
// SQLite 与加密实现（15-render-pipeline §9）。
type RenderStore struct {
	ctx         context.Context
	v           *Vault
	componentID int64
}

// NewRenderStore 为某个 Component 构造密钥入口。
func NewRenderStore(ctx context.Context, v *Vault, componentID int64) *RenderStore {
	return &RenderStore{ctx: ctx, v: v, componentID: componentID}
}

var _ render.SecretStore = (*RenderStore)(nil)

// Ensure 实现「仅首次生成」。
func (s *RenderStore) Ensure(component, param string, g pack.Generate) (render.StoredSecret, error) {
	value, version, _, err := s.v.Generate(s.ctx, s.componentID, param, g)
	if err != nil {
		return render.StoredSecret{}, err
	}
	return render.StoredSecret{ID: s.idFor(param), Version: version, Value: value}, nil
}

// Store 固化一个用户给的敏感值。
//
// **值没变就不写**：Vault.Put 每次都会自增版本，而版本参与 spec digest。
// 无条件写的后果是每轮调和都产生一个新 generation、重渲染、重启服务——
// 一个什么都没改的部署会永远滚动下去。
func (s *RenderStore) Store(component, param, value string) (render.StoredSecret, error) {
	cur, version, ok, err := s.v.Get(s.ctx, s.componentID, param)
	if err != nil {
		return render.StoredSecret{}, err
	}
	if ok && cur == value {
		return render.StoredSecret{ID: s.idFor(param), Version: version, Value: value}, nil
	}
	version, err = s.v.Put(s.ctx, s.componentID, param, value)
	if err != nil {
		return render.StoredSecret{}, err
	}
	return render.StoredSecret{ID: s.idFor(param), Version: version, Value: value}, nil
}

// idFor 给一条密钥一个**稳定**的引用 id。
//
// 稳定是硬要求：id 进 digest，每次渲染换一个会让 digest 每次都变。
// 用 (componentID, param) 而非随机数正是为此——同一个 Component 的
// 同一个参数在任何时候都指向同一条记录。
func (s *RenderStore) idFor(param string) string {
	return fmt.Sprintf("c%d.%s", s.componentID, param)
}
