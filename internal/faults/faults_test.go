package faults

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestClassOfDefaultsToPermanent 钉住「未分类的错误按不可重试处理」。
//
// 反过来（默认可重试）会让一个配置错误在 60 秒一轮的调和里无限重试，
// 而且每一轮都会真的动手改机器。默认暂停并让人来看，代价小得多。
func TestClassOfDefaultsToPermanent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"裸错误", errors.New("出事了")},
		{"nil", nil},
		{"包装过的裸错误", fmt.Errorf("上层: %w", errors.New("底层"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassOf(tc.err); got != Permanent {
				t.Errorf("ClassOf = %s，期望 permanent", got)
			}
		})
	}
}

func TestClassSurvivesWrapping(t *testing.T) {
	base := Transientf("读取", "超时")
	wrapped := fmt.Errorf("调和 pg-main: %w", base)

	if !IsTransient(wrapped) {
		t.Error("分类应当能穿过 fmt.Errorf 的包装被识别")
	}
	if !strings.Contains(wrapped.Error(), "读取: 超时") {
		t.Errorf("错误信息 = %q", wrapped.Error())
	}
}

func TestWrapPreservesNil(t *testing.T) {
	if err := Wrap(Transient, "读取", nil); err != nil {
		t.Errorf("包装 nil 应当得到 nil，实际 %v", err)
	}
}

func TestUnwrap(t *testing.T) {
	sentinel := errors.New("哨兵")
	err := Wrap(Permanent, "操作", sentinel)

	if !errors.Is(err, sentinel) {
		t.Error("应当能用 errors.Is 找到原始错误")
	}
	var fe *Error
	if !errors.As(err, &fe) || fe.Op != "操作" {
		t.Errorf("应当能取出 Op，实际 %+v", fe)
	}
}

func TestErrorWithoutOp(t *testing.T) {
	err := &Error{Class: Permanent, Err: errors.New("光秃秃的")}
	if err.Error() != "光秃秃的" {
		t.Errorf("没有 Op 时不该多出一个冒号: %q", err.Error())
	}
}

func TestClassString(t *testing.T) {
	if Transient.String() != "transient" || Permanent.String() != "permanent" {
		t.Error("分类名会出现在给用户看的输出里，必须稳定")
	}
}
