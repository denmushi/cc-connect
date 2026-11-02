package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type delayedIDAgent struct {
	stubAgent
	kind      string
	eventKind string
	mu        sync.Mutex
	starts    []string
}

func (a *delayedIDAgent) Name() string { return a.kind }
func (a *delayedIDAgent) StartSession(_ context.Context, resume string) (AgentSession, error) {
	a.mu.Lock()
	a.starts = append(a.starts, resume)
	a.mu.Unlock()
	s := &delayedIDSession{target: a.kind + "-native-id", eventKind: a.eventKind, events: make(chan Event, 8)}
	s.id.Store(resume)
	s.alive.Store(true)
	return s, nil
}
func (a *delayedIDAgent) requestedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.starts...)
}

type delayedIDSession struct {
	stubAgentSession
	target    string
	eventKind string
	id        atomic.Value
	alive     atomic.Bool
	events    chan Event
}

func (s *delayedIDSession) CurrentSessionID() string { return s.id.Load().(string) }
func (s *delayedIDSession) Alive() bool              { return s.alive.Load() }
func (s *delayedIDSession) Close() error             { s.alive.Store(false); return nil }
func (s *delayedIDSession) Events() <-chan Event     { return s.events }
func (s *delayedIDSession) Send(_ string, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	// 模拟 Claude 的延迟上报：新进程返回后，原生 ID 才随启动、完成或错误事件出现。
	s.id.Store(s.target)
	if s.eventKind == "text" {
		s.events <- Event{Type: EventText, SessionID: s.target}
	}
	if s.eventKind == "error" {
		s.events <- Event{Type: EventError, Error: fmt.Errorf("synthetic error after native session creation")}
		return nil
	}
	s.events <- Event{Type: EventResult, SessionID: s.target, Content: "ok", Done: true}
	return nil
}

func assertSessionAgentType(t *testing.T, session *Session, kind, id string) {
	t.Helper()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.AgentType != kind || session.AgentSessionID != id || len(session.PastAgentSessionIDs) != 0 {
		t.Fatalf("session identity: type=%q id=%q past=%v; want type=%q id=%q", session.AgentType, session.AgentSessionID, session.PastAgentSessionIDs, kind, id)
	}
}

func TestGroupAgentTypeUsesActiveAgent(t *testing.T) {
	for _, selected := range []string{"claudecode", "codex"} {
		for _, eventKind := range []string{"text", "result", "error"} {
			t.Run(selected+"/"+eventKind, func(t *testing.T) {
				defaultKind, command := "codex", "/cc-create"
				if selected == "codex" {
					defaultKind, command = "claudecode", "/codex-create"
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "group-agents.json")
				makeEngine := func() (*Engine, *delayedIDAgent, *delayedIDAgent) {
					fallback := &delayedIDAgent{kind: defaultKind, eventKind: "text"}
					active := &delayedIDAgent{kind: selected, eventKind: eventKind}
					e := NewEngine("bot", fallback, nil, filepath.Join(dir, "default-sessions.json"), LangChinese)
					e.SetReplyFooterEnabled(false)
					if err := e.SetGroupAgents(map[string]Agent{defaultKind: fallback, selected: active}, path); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = e.Stop() })
					return e, fallback, active
				}
				platform := &stubPlatformEngine{n: "feishu"}
				key := "feishu:fixture-group"
				e, _, _ := makeEngine()
				e.handleMessage(platform, &Message{SessionKey: key, Platform: "feishu", Content: command})
				session := waitGroupTurn(t, e, key)
				id := selected + "-native-id"
				assertSessionAgentType(t, session, selected, id)

				// 旧记录可能在原进程里继续使用；即使 ID 没变也要修正类型。
				session.SetAgentSessionID(id, defaultKind)
				e.handleMessage(platform, &Message{SessionKey: key, Platform: "feishu", Content: "继续"})
				assertSessionAgentType(t, waitGroupTurn(t, e, key), selected, id)
				if err := e.Stop(); err != nil {
					t.Fatal(err)
				}
				storePath := path + "." + selected + ".sessions.json"
				store := NewSessionManager(storePath)
				assertSessionAgentType(t, store.GetOrCreateActive(key), selected, id)

				// 模拟磁盘上的旧错误标签，恢复时必须保留原生 ID 与群路由。
				store.GetOrCreateActive(key).SetAgentSessionID(id, defaultKind)
				store.Save()
				restarted, fallback, active := makeEngine()
				routed, sessions := restarted.groupAgents.context(key)
				if routed.Name() != selected || sessions.GetOrCreateActive(key).ContextScope != selected {
					t.Fatal("group routing or context scope changed")
				}
				// 只恢复进程、不发送模型回合，也应完成旧标签修复。
				restarted.getOrCreateInteractiveStateWith(key, platform, nil, sessions.GetOrCreateActive(key), sessions, active, key)
				assertSessionAgentType(t, sessions.GetOrCreateActive(key), selected, id)
				assertSessionAgentType(t, NewSessionManager(storePath).GetOrCreateActive(key), selected, id)
				if got := active.requestedIDs(); len(got) != 1 || got[0] != id || len(fallback.requestedIDs()) != 0 {
					t.Fatalf("wrong backend/ID on resume: active=%v fallback=%v", got, fallback.requestedIDs())
				}
			})
		}
	}
}
