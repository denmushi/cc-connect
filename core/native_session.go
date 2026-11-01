package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

type nativeSessionNameKey struct{}

// WithNativeSessionName 随本次启动传递标题，避免共享 Agent 在并发群聊间串名字。
func WithNativeSessionName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, nativeSessionNameKey{}, strings.TrimSpace(name))
}

func NativeSessionName(ctx context.Context) string {
	name, _ := ctx.Value(nativeSessionNameKey{}).(string)
	return name
}

func nativeSessionContext(ctx context.Context, session *Session) context.Context {
	if session.ContextScope != "" {
		return WithNativeSessionName(ctx, session.GetName())
	}
	return ctx
}

// GroupSessionNamer 让平台提供用户可辨认的群名和机器人名，不把提示词当标题。
type GroupSessionNamer interface {
	GroupSessionName(*Message) string
}

// NativeDesktopLauncher 返回原生客户端的启动参数，不经过 shell，也不把凭证暴露给 API。
type NativeDesktopLauncher interface {
	NativeDesktopCommand(sessionID string) (*NativeDesktopCommand, error)
}

type NativeDesktopCommand struct {
	Args []string
	Dir  string
	Env  []string
	PTY  bool
}

func (c *NativeDesktopCommand) Run(ctx context.Context) error {
	if len(c.Args) == 0 {
		return fmt.Errorf("empty desktop command")
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = MergeEnv(os.Environ(), c.Env)
	if c.PTY {
		return runNativeDesktopPTY(cmd)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("打开原生客户端失败：%w：%s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Claude --desktop 要求真实 TTY；后台 daemon 的管道输出会被它判为非交互模式。
func runNativeDesktopPTY(cmd *exec.Cmd) error {
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		return fmt.Errorf("打开原生客户端终端失败：%w", err)
	}
	defer terminal.Close()
	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 持续排空 PTY，但只保留有限诊断输出，避免子进程阻塞或无限占内存。
		_, _ = io.CopyN(&captured, terminal, 64*1024)
		_, _ = io.Copy(io.Discard, terminal)
	}()
	err = cmd.Wait()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		_ = terminal.Close()
		<-done
	}
	if err != nil {
		return fmt.Errorf("打开原生客户端失败：%w：%s", err, strings.TrimSpace(captured.String()))
	}
	return nil
}
