//go:build unix

package command

import (
	"os/exec"
	"syscall"
)

// setCredential 让子进程以指定 uid/gid 运行。
//
// 用 SysProcAttr 而不是包一层 su / runuser：后者要多 fork 一个 shell，
// 而那个 shell 会自己动环境变量（su 甚至默认重置 PATH 与 HOME），
// 让「hook 拿到的环境」变得不可预期——那正是最难查的一类差异。
func setCredential(cmd *exec.Cmd, uid, gid int) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid),
	}
	return nil
}

// setProcessGroup 让命令在自己的进程组里运行，从而可以整组杀掉。
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup 杀掉整个进程组。
//
// 负的 pid 表示进程组（kill(2)）。先 TERM 再 KILL 是常见做法，但这里
// 直接 KILL：走到这一步说明已经超时，再给一次优雅退出的机会只会让
// 超时形同虚设。
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// 进程组可能已经没了（正常退出与超时竞争）；退回杀单个进程
		return cmd.Process.Kill()
	}
	return nil
}
