package claudecode

import (
	"fmt"
	"runtime"

	"github.com/chenhg5/cc-connect/core"
)

func (a *Agent) NativeDesktopCommand(id string) (*core.NativeDesktopCommand, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("当前系统不支持 Claude Desktop 接管")
	}
	if id == "" {
		return nil, fmt.Errorf("原生会话 ID 为空")
	}
	if a.spawnOpts.IsolationMode() || a.cmdArgsFlag != "" {
		return nil, fmt.Errorf("隔离用户或 CLI 包装器需要手动接管，原群会话保持运行")
	}
	args := append([]string{a.cmd}, a.cliExtraArgs...)
	args = append(args, "--desktop", "--resume", id)
	return &core.NativeDesktopCommand{Args: args, Dir: a.workDir, Env: a.runtimeEnvLocked(), PTY: true}, nil
}
