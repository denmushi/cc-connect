package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// groupAgentRouting 保存每个群的选择；不同 Agent 的会话文件独立，避免重启或切换时串会话。
type groupAgentRouting struct {
	mu       sync.Mutex
	locks    sync.Map
	agents   map[string]Agent
	sessions map[string]*SessionManager
	bindings map[string]string
	path     string
}

func (e *Engine) SetGroupAgents(agents map[string]Agent, path string) error {
	r := &groupAgentRouting{agents: agents, sessions: make(map[string]*SessionManager), bindings: make(map[string]string), path: path}
	for name := range agents {
		if name != "codex" && name != "claudecode" {
			return fmt.Errorf("unsupported group agent %q", name)
		}
		r.sessions[name] = NewSessionManager(path + "." + name + ".sessions.json")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &r.bindings); err != nil {
			return fmt.Errorf("read group agent bindings: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if r.bindings == nil {
		r.bindings = make(map[string]string)
	}
	e.groupAgents = r
	return nil
}

func (r *groupAgentRouting) lock(key string) func() {
	value, _ := r.locks.LoadOrStore(key, &sync.Mutex{})
	m := value.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func (r *groupAgentRouting) context(key string) (Agent, *SessionManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := r.bindings[key]
	return r.agents[name], r.sessions[name]
}

func (r *groupAgentRouting) bind(key, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]string, len(r.bindings)+1)
	for k, v := range r.bindings {
		next[k] = v
	}
	next[key] = name
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(r.path+".tmp", data, 0600); err != nil {
		return err
	}
	if err = os.Rename(r.path+".tmp", r.path); err != nil {
		return err
	}
	r.bindings = next
	return nil
}

func (r *groupAgentRouting) hint() string {
	var commands []string
	if r.agents["codex"] != nil {
		commands = append(commands, "/codex-create")
	}
	if r.agents["claudecode"] != nil {
		commands = append(commands, "/cc-create")
	}
	return "请主人先用 " + strings.Join(commands, " 或 ") + " 在本群新建并绑定会话，之后直接用自然语言交流。"
}

// handleGroupAgentEntry 只保留两个创建入口；在别名、shell 快捷方式和旧命令解析前处理。
// 调用方持有群锁，直到普通消息取得会话忙锁，避免创建命令与新一轮消息抢占绑定。
func (e *Engine) handleGroupAgentEntry(p Platform, msg *Message) bool {
	r := e.groupAgents
	fields := strings.Fields(msg.Content)
	command := ""
	if len(fields) > 0 {
		command = fields[0]
	}
	name := map[string]string{"/codex-create": "codex", "/cc-create": "claudecode"}[command]
	if name == "" {
		if strings.HasPrefix(command, "/") || strings.HasPrefix(command, "!") {
			e.reply(p, msg.ReplyCtx, "当前仅支持创建会话命令。"+r.hint())
			return true
		}
		if agent, _ := r.context(msg.SessionKey); agent == nil {
			e.reply(p, msg.ReplyCtx, r.hint())
			return true
		}
		return false
	}
	if msg.CannotApprove {
		e.reply(p, msg.ReplyCtx, "只有机器人的主人能创建和绑定会话。")
		return true
	}
	if len(fields) != 1 {
		e.reply(p, msg.ReplyCtx, "请单独发送 "+command+"，创建完成后再发送要讨论的内容。")
		return true
	}
	if r.agents[name] == nil {
		e.reply(p, msg.ReplyCtx, "本机尚未配置这个 Agent。"+r.hint())
		return true
	}
	if _, previous := r.context(msg.SessionKey); previous != nil && previous.GetOrCreateActive(msg.SessionKey).Busy() {
		e.reply(p, msg.ReplyCtx, "当前会话还在回复，请等本轮结束后再创建新会话。")
		return true
	}
	// 先持久化路由；保存失败时继续保留原绑定，不向用户报告成功。
	if err := r.bind(msg.SessionKey, name); err != nil {
		e.reply(p, msg.ReplyCtx, "保存会话绑定失败："+err.Error())
		return true
	}
	e.cleanupInteractiveState(msg.SessionKey)
	sm := r.sessions[name]
	session := sm.NewSession(msg.SessionKey, "群聊会话")
	session.ContextScope = name
	sm.Save()
	// 创建命令触发一个最小真实回合，使 Agent 原生会话落盘，而不只创建桥接层占位。
	gen, ok := session.TryLock()
	if !ok {
		return true
	}
	copy := *msg
	label := map[string]string{"codex": "Codex", "claudecode": "Claude Code"}[name]
	copy.Content = "这是主人刚通过 " + command + " 创建并绑定的全新群聊会话。不要执行群历史中的旧任务，不调用工具。只回复一句：已创建并绑定新的 " + label + " 会话，可以直接开始交流。"
	go e.processInteractiveMessageWith(p, &copy, session, r.agents[name], sm, msg.SessionKey, "", msg.SessionKey, gen)
	return true
}
