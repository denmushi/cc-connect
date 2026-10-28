package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type groupTestAgent struct {
	stubAgent
	kind   string
	mu     sync.Mutex
	starts []string
}

func (a *groupTestAgent) Name() string { return a.kind }
func (a *groupTestAgent) StartSession(_ context.Context, id string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts = append(a.starts, id)
	if id == "" {
		id = fmt.Sprintf("%s-%d", a.kind, len(a.starts))
	}
	return &groupTestSession{id: id, events: make(chan Event, 8)}, nil
}

type groupTestSession struct {
	stubAgentSession
	id     string
	events chan Event
}

func (s *groupTestSession) CurrentSessionID() string { return s.id }
func (s *groupTestSession) Events() <-chan Event     { return s.events }
func (s *groupTestSession) Send(text, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.events <- Event{Type: EventText, Content: s.id + ":" + text}
	s.events <- Event{Type: EventResult, SessionID: s.id, Done: true}
	return nil
}

func waitGroupTurn(t *testing.T, e *Engine, key string) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, sm := e.groupAgents.context(key)
		if sm != nil {
			s := sm.GetOrCreateActive(key)
			if !s.Busy() && s.GetAgentSessionID() != "" {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("group turn did not finish")
	return nil
}

func TestGroupAgentsCreateRouteAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	codex := &groupTestAgent{kind: "codex"}
	cc := &groupTestAgent{kind: "claudecode"}
	agents := map[string]Agent{"codex": codex, "claudecode": cc}
	newEngine := func() *Engine {
		e := NewEngine("group", codex, nil, "", LangChinese)
		e.SetReplyFooterEnabled(false)
		if err := e.SetGroupAgents(agents, path); err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := newEngine()
	p := &stubPlatformEngine{n: "feishu"}
	send := func(key, text string) {
		e.handleMessage(p, &Message{SessionKey: key, Platform: "feishu", Content: text})
	}
	send("group-a", "/codex-create")
	old := waitGroupTurn(t, e, "group-a")
	send("group-b", "/cc-create")
	other := waitGroupTurn(t, e, "group-b")
	send("group-a", "/cc-create")
	current := waitGroupTurn(t, e, "group-a")
	if old.GetAgentSessionID() != "codex-1" || other == current || current.ContextScope != "claudecode" {
		t.Fatal("old history or group isolation lost")
	}
	send("group-a", "自然语言分工")
	waitGroupTurn(t, e, "group-a")
	if got := current.GetHistory(0); len(got) < 4 {
		t.Fatalf("missing continued history: %v", got)
	}
	id := current.GetAgentSessionID()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	e = newEngine()
	defer e.Stop()
	agent, sm := e.groupAgents.context("group-a")
	if agent.Name() != "claudecode" || sm.GetOrCreateActive("group-a").GetAgentSessionID() != id || sm.GetOrCreateActive("group-a").ContextScope != "claudecode" {
		t.Fatal("binding/context did not survive restart")
	}
	send("group-a", "重启后继续")
	waitGroupTurn(t, e, "group-a")
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.starts[len(cc.starts)-1] != id {
		t.Fatal("resumed wrong native session")
	}
}

func TestGroupAgentsOnlyCreateCommandsAndOwner(t *testing.T) {
	a := &groupTestAgent{kind: "codex"}
	e := NewEngine("group", a, nil, "", LangChinese)
	defer e.Stop()
	if err := e.SetGroupAgents(map[string]Agent{"codex": a}, filepath.Join(t.TempDir(), "bindings.json")); err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "feishu"}
	e.aliases["帮助"] = "/new"
	for _, text := range []string{"你好", "/list", "/role 后端", "/switch 1", "/new", "!echo bad", "/cc-create", "/codex-create extra"} {
		e.handleMessage(p, &Message{SessionKey: "g", Content: text})
	}
	e.handleMessage(p, &Message{SessionKey: "g", Content: "/codex-create", CannotApprove: true})
	if ag, _ := e.groupAgents.context("g"); ag != nil {
		t.Fatal("unexpected binding")
	}
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "只有机器人的主人") {
		t.Fatal("owner rejection missing")
	}
	e.handleMessage(p, &Message{SessionKey: "g", Content: "/codex-create"})
	s := waitGroupTurn(t, e, "g")
	gen, _ := s.TryLock()
	e.handleMessage(p, &Message{SessionKey: "g", Content: "/codex-create"})
	s.Unlock(gen)
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "还在回复") {
		t.Fatal("busy session was replaced")
	}
	e.handleMessage(p, &Message{SessionKey: "g", Content: "帮助"})
	if waitGroupTurn(t, e, "g") != s {
		t.Fatal("alias executed as command")
	}
}
