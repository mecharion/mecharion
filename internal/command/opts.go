package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// waitDelay 是杀掉进程组之后，仍然等待 Wait 收摊的宽限。
//
// 有进程能逃出进程组（自己 setsid），那时只能靠这层兜底强行关掉管道。
const waitDelay = 5 * time.Second

// Opts 是一次执行的额外设置。
//
// 单独立出来而不是加进 Run 的签名：绝大多数调用（getent / systemctl）
// 什么都不需要，让它们全都写一个空结构体只是噪音。
type Opts struct {
	// Dir 是工作目录。
	Dir string
	// Env 是**完整**的环境变量表，为空时继承当前进程。
	//
	// 是替换而非追加：hook 的环境必须是可预期的，继承一份开发机上恰好
	// 存在的变量会让「在我这儿能跑」变成常态。
	Env []string
	// User 是运行身份的用户名，为空则不切换。
	User string
	// UID / GID 在 User 非空时由调用方查好——身份查询要走 getent，
	// 那是调用方的上下文，不是这一层的。
	UID, GID int
	// Stdin 是喂给命令的标准输入。
	Stdin string
}

// RunWith 在给定设置下执行命令。
func (Exec) RunWith(
	ctx context.Context, o Opts, name string, args ...string,
) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	if o.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(o.Stdin))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if o.User != "" {
		if err := setCredential(cmd, o.UID, o.GID); err != nil {
			return Result{}, fmt.Errorf("以 %s 身份执行 %s: %w", o.User, name, err)
		}
	}

	// **超时必须杀掉整棵进程树，不只是那个脚本。**
	//
	// 脚本 fork 出的子进程会继承 stdout 的管道写端。只杀脚本的话，
	// 子进程仍握着管道，cmd.Wait() 就一直等在读端上——一个
	// `sleep 30 &` 足以让 300 秒的 timeout 形同虚设，而现象是「超时了
	// 但函数没返回」，最难查的那一类。
	//
	// 做法：把命令放进自己的进程组，超时时杀整组；WaitDelay 再兜一层，
	// 防止有进程逃出进程组（setsid 之类）后仍然吊住 Wait。
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		return res, nil
	default:
		return res, err
	}
}
