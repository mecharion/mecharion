package mechd

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mecharion/mecharion/internal/ident"
)

// TestDeployRejectsInvalidComponentName 与 TestAddNodeRejectsInvalidName
// 从 Service 这一层（而不是直接调 internal/store）验证校验的落地效果：
// 校验发生在全仓库唯一能落库的地方，但用户实际打交道的入口是
// Service.Deploy / Service.AddNode，这里确认错误能一路传上来，并且
// 通过 mechd 自己的 statusFor 判据映射到 400（而不是被当成服务端故障
// 报 500）——这是从 API 一侧对该校验的验收。
func TestDeployRejectsInvalidComponentName(t *testing.T) {
	f := newFixture(t, "n1")

	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "go-webapp", Component: "../evil",
		Roles: map[string][]string{"default": {"n1"}},
	})
	if err == nil {
		t.Fatal("非法 Component 名应当被拒绝，实际部署成功")
	}
	if !errors.Is(err, ident.ErrInvalid) {
		t.Errorf("错误 %v 未包裹 ident.ErrInvalid", err)
	}
	if got := statusFor(err); got != http.StatusBadRequest {
		t.Errorf("statusFor = %d，期望 400（否则会被当成服务端故障报 500）", got)
	}
}

func TestAddNodeRejectsInvalidName(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.AddNode(ctx(), f.site.Name, "not a valid name", "10.0.0.1", "test")
	if err == nil {
		t.Fatal("非法 Node 名应当被拒绝，实际登记成功")
	}
	if !errors.Is(err, ident.ErrInvalid) {
		t.Errorf("错误 %v 未包裹 ident.ErrInvalid", err)
	}
	if got := statusFor(err); got != http.StatusBadRequest {
		t.Errorf("statusFor = %d，期望 400", got)
	}
}
