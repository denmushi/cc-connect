package codex

import (
	"fmt"
	"net/url"
	"runtime"

	"github.com/chenhg5/cc-connect/core"
)

func (a *Agent) NativeDesktopCommand(id string) (*core.NativeDesktopCommand, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("自动打开 Codex Desktop 目前仅支持 macOS")
	}
	if id == "" {
		return nil, fmt.Errorf("原生会话 ID 为空")
	}
	return &core.NativeDesktopCommand{Args: []string{"open", "codex://threads/" + url.PathEscape(id)}}, nil
}
