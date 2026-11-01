package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func runDesktop(args []string) {
	if err := desktopCommand(args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func desktopCommand(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(out, `用法：cc-connect desktop list [--config 路径]
      cc-connect desktop open <原生会话ID> [--config 路径]
      cc-connect desktop resume <原生会话ID> [--config 路径]
      cc-connect desktop repair <Codex会话ID> [--config 路径]

open：等待当前回合结束后使用，暂停该群会话并打开原生客户端。
resume：先关闭原生客户端中的该会话，再恢复飞书接收；沿用同一个会话ID。
repair：补齐已有 Codex 会话的项目关联和空标题，不发送群消息。
Claude Desktop 需要支持 --desktop 的 Claude Code，并使用 Claude 订阅登录。`)
		return nil
	}
	action := args[0]
	if action != "list" && action != "open" && action != "resume" && action != "repair" {
		return fmt.Errorf("不支持的操作 %q", action)
	}
	id := ""
	args = args[1:]
	if action != "list" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("请先运行 desktop list，指定原生会话 ID")
		}
		id, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("desktop", flag.ContinueOnError)
	flags.SetOutput(out)
	configPath := flags.String("config", "", "配置文件")
	dataDir := flags.String("data-dir", "", "数据目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("多余参数：%s", strings.Join(flags.Args(), " "))
	}
	if *dataDir == "" {
		cfg, err := config.Load(resolveConfigPath(*configPath))
		if err != nil {
			return fmt.Errorf("读取配置：%w", err)
		}
		*dataDir = cfg.DataDir
	}
	socket := filepath.Join(*dataDir, "run", "api.sock")
	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	var response *http.Response
	var err error
	if action == "list" {
		response, err = client.Get("http://unix/desktop")
	} else {
		body, _ := json.Marshal(core.DesktopRequest{Action: action, SessionID: id})
		response, err = client.Post("http://unix/desktop", "application/json", bytes.NewReader(body))
	}
	if err != nil {
		return fmt.Errorf("连接本机 cc-connect 服务失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	if action == "list" {
		var rows []core.DesktopSession
		if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
			return err
		}
		for _, row := range rows {
			state := "飞书接收中"
			if row.Paused {
				state = "桌面接管／已暂停"
			}
			fmt.Fprintf(out, "%s  %s  %s  [%s]\n  %s\n", row.SessionID, row.Agent, row.Name, state, row.WorkDir)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "还没有已绑定的原生会话。请在群内先创建。")
		}
	} else {
		messages := map[string]string{"open": "已请求原生客户端打开；该群的飞书接收已暂停。", "resume": "已恢复飞书接收，下一条消息继续同一会话。", "repair": "已补齐 Codex 原生会话元数据。"}
		fmt.Fprintln(out, messages[action])
	}
	return nil
}
