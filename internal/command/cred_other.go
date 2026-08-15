//go:build !unix

package command

import (
	"fmt"
	"os/exec"
)

// setCredential 在非 Unix 平台上不可用。
//
// 明确报错而不是静默忽略：静默会让一个本该以 postgres 身份跑的 hook
// 以 root 跑起来，而那要到出事时才被发现。
func setCredential(_ *exec.Cmd, _, _ int) error {
	return fmt.Errorf("本平台不支持切换执行身份（hook 的 user 字段仅在 Unix 上可用）")
}

// setProcessGroup 在非 Unix 平台上无操作。
func setProcessGroup(_ *exec.Cmd) {}

// killGroup 在非 Unix 平台上退回杀单个进程。
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
