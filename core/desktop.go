package core

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type DesktopSession struct {
	Project    string `json:"project"`
	SessionKey string `json:"session_key"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Name       string `json:"name"`
	WorkDir    string `json:"work_dir"`
	Paused     bool   `json:"paused"`
}

func (e *Engine) DesktopSessions() []DesktopSession {
	r := e.groupAgents
	if r == nil {
		return nil
	}
	r.mu.Lock()
	keys := make([]string, 0, len(r.bindings))
	for key := range r.bindings {
		keys = append(keys, key)
	}
	r.mu.Unlock()
	var rows []DesktopSession
	for _, key := range keys {
		agent, sm := r.context(key)
		if agent == nil || sm == nil {
			continue
		}
		sm.mu.RLock()
		session := sm.sessions[sm.activeSession[key]]
		sm.mu.RUnlock()
		if session == nil || session.GetAgentSessionID() == "" {
			continue
		}
		row := DesktopSession{Project: e.name, SessionKey: key, SessionID: session.GetAgentSessionID(), Agent: agent.Name(), Name: session.GetName(), Paused: session.IsDesktopPaused()}
		if wd, ok := agent.(WorkDirSwitcher); ok {
			row.WorkDir = wd.GetWorkDir()
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SessionID < rows[j].SessionID })
	return rows
}

// DesktopSessionAction 与群消息共用群锁。先持久化暂停，再确认进程退出，最后交给桌面。
func (e *Engine) DesktopSessionAction(ctx context.Context, row DesktopSession, action string) error {
	r := e.groupAgents
	if r == nil {
		return fmt.Errorf("未配置群协作")
	}
	unlock := r.lock(row.SessionKey)
	defer unlock()
	agent, sm := r.context(row.SessionKey)
	if agent == nil || sm == nil {
		return fmt.Errorf("群绑定已变化，请重新列出会话")
	}
	session := sm.GetOrCreateActive(row.SessionKey)
	if session.GetAgentSessionID() != row.SessionID {
		return fmt.Errorf("群绑定已变化，请重新列出会话")
	}
	if action != "open" && action != "resume" && action != "repair" {
		return fmt.Errorf("不支持的操作 %q", action)
	}
	if session.Busy() {
		return fmt.Errorf("当前会话还在回复，请等本轮结束")
	}
	if action == "resume" {
		if !session.IsDesktopPaused() {
			return nil
		}
		session.SetDesktopPaused(false)
		if err := sm.SaveChecked(); err != nil {
			session.SetDesktopPaused(true)
			return err
		}
		return nil
	}
	if session.IsDesktopPaused() {
		return fmt.Errorf("已暂停给原生客户端；关闭原生会话后运行 desktop resume 恢复")
	}
	var command *NativeDesktopCommand
	if action == "open" {
		launcher, ok := agent.(NativeDesktopLauncher)
		if !ok {
			return fmt.Errorf("该 Agent 不支持桌面接管")
		}
		var err error
		command, err = launcher.NativeDesktopCommand(row.SessionID)
		if err != nil {
			return err
		}
	} else if agent.Name() != "codex" {
		return fmt.Errorf("repair 仅用于补齐 Codex 项目及标题；Claude Code 在新建时自动命名")
	}
	session.SetDesktopPaused(true)
	if err := sm.SaveChecked(); err != nil {
		session.SetDesktopPaused(false)
		return err
	}
	// Close 必须真正结束子进程；普通 /stop 可能仅取消回合，不适合移交所有权。
	e.interactiveMu.Lock()
	state := e.interactiveStates[row.SessionKey]
	if state != nil {
		e.stopUnsolicitedReader(state)
		state.markStopped()
		delete(e.interactiveStates, row.SessionKey)
	}
	e.interactiveMu.Unlock()
	if state != nil && state.agentSession != nil {
		if err := state.agentSession.Close(); err != nil {
			e.markUnsafeResume(row.SessionKey)
			return fmt.Errorf("会话已暂停，但进程退出未确认：%w", err)
		}
	}
	if action == "repair" {
		// 只恢复原生记录并同步元数据，不发送模型回合或群消息。
		name := session.GetName()
		if name == "群聊会话" {
			if meta := sm.GetUserMeta(row.SessionKey); meta != nil && meta.ChatName != "" {
				name = meta.ChatName + " · Codex"
				platform, _, _ := ParseSessionKey(row.SessionKey)
				if namer, ok := e.platformForName(platform).(GroupSessionNamer); ok {
					name = namer.GroupSessionName(&Message{ChatName: meta.ChatName}) + " · Codex"
				}
			}
		}
		native, err := agent.StartSession(WithNativeSessionName(ctx, name), row.SessionID)
		if err != nil {
			return fmt.Errorf("修复失败，群会话保持暂停：%w", err)
		}
		if err := native.Close(); err != nil {
			e.markUnsafeResume(row.SessionKey)
			return err
		}
		if check, ok := native.(interface{ NativeMetadataError() error }); ok && check.NativeMetadataError() != nil {
			return fmt.Errorf("元数据未完全同步，群会话保持暂停：%w", check.NativeMetadataError())
		}
		session.SetName(name)
		sm.SetSessionName(row.SessionID, name)
		session.SetDesktopPaused(false)
		if err := sm.SaveChecked(); err != nil {
			session.SetDesktopPaused(true)
			return err
		}
		return nil
	}
	launchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := command.Run(launchCtx); err != nil {
		return fmt.Errorf("%w；群会话保持暂停，处理后运行 desktop resume 恢复", err)
	}
	return nil
}
