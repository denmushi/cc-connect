package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	ccconnect "github.com/chenhg5/cc-connect"
	"golang.org/x/term"
)

type setupRuntime struct {
	binary string
	args   []string
}

func condaPython(prefix string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(prefix, "python.exe")
	}
	return filepath.Join(prefix, "bin", "python")
}

func compatibleSetupPython(prefix string) bool {
	if prefix == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(prefix, "conda-meta", "history")); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, condaPython(prefix), "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)")
	return cmd.Run() == nil
}

// 优先复用已激活的 Conda；其次查找已存在的兼容环境，不创建环境或退回系统 Python。
func findSetupRuntime() (setupRuntime, error) {
	if prefix := os.Getenv("CONDA_PREFIX"); compatibleSetupPython(prefix) {
		return setupRuntime{binary: condaPython(prefix)}, nil
	}
	conda, err := exec.LookPath("conda")
	if err != nil {
		return setupRuntime{}, fmt.Errorf("配置尚未完成：请先安装或激活 Python 3.11+ 的 Conda 环境，再运行 cc-connect setup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, conda, "env", "list", "--json").Output()
	if err != nil {
		return setupRuntime{}, fmt.Errorf("无法查询 Conda 环境，请激活 Python 3.11+ 环境后重试 cc-connect setup")
	}
	var envs struct {
		Envs []string `json:"envs"`
	}
	if err := json.Unmarshal(out, &envs); err != nil {
		return setupRuntime{}, err
	}
	for _, prefix := range envs.Envs {
		if compatibleSetupPython(prefix) {
			return setupRuntime{binary: conda, args: []string{"run", "--no-capture-output", "-p", prefix, "python"}}, nil
		}
	}
	return setupRuntime{}, fmt.Errorf("未找到兼容的 Conda 环境。可先执行 conda create -n cc-connect python=3.11，再运行 cc-connect setup")
}

func executeSetup(args []string, in io.Reader, out, errOut io.Writer) error {
	python, err := findSetupRuntime()
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "cc-connect-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{"feishu-collab-setup.py", "collab_config.py"} {
		data, err := ccconnect.SetupAssets.ReadFile("tools/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	argv := append(append([]string{}, python.args...), filepath.Join(dir, "feishu-collab-setup.py"))
	argv = append(argv, args...)
	argv = append(argv, "--installed-binary", bin)
	cmd := exec.Command(python.binary, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("配置向导未完成，请按上面的提示处理后重新运行 cc-connect setup：%w", err)
	}
	return nil
}

func runSetup(args []string) {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Println("用法：cc-connect setup [--add-bot] [--home 目录] [--agents codex,cc] [--codex-dir 目录] [--cc-dir 目录] [--no-service]\n\n安装二进制后继续运行此向导，逐项选择 Agent、确认工作目录、配置机器人并启动服务。\n默认目录会显示在问题中，可回车接受或输入自定义路径；setup --add-bot 复用已确认的 Agent 配置。\n需要已有的 Python 3.11+ Conda 环境；不需要源码或 Go 编译器。")
			return
		}
	}
	if err := executeSetup(args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// 后台服务和非交互执行不得创建占位配置或自动采用默认答案。
func canStartSetup(interactive bool, daemonLog string) bool {
	return interactive && strings.TrimSpace(daemonLog) == ""
}

func missingConfigSetup(explicit string) {
	if !canStartSetup(term.IsTerminal(int(os.Stdin.Fd())), os.Getenv("CC_LOG_FILE")) {
		fmt.Fprintln(os.Stderr, "程序已安装，但配置尚未完成。请在交互终端运行 cc-connect setup，继续选择 Agent、确认目录、配置机器人和启动服务。")
		os.Exit(2)
	}
	args := []string{}
	if explicit != "" {
		if filepath.Base(explicit) != "config.toml" {
			fmt.Fprintln(os.Stderr, "配置文件不存在。请运行 cc-connect setup 完成配置，或提供现有的 --config 文件。")
			os.Exit(2)
		}
		args = append(args, "--home", filepath.Dir(explicit))
	}
	fmt.Println("首次运行，继续完成配置。每个目录都会显示默认路径，请自行选择。")
	runSetup(args)
}
