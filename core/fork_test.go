package core

import (
	"context"
	"testing"
)

type forkingAgent struct {
	stubAgent
	started, forked []string
}

func (a *forkingAgent) StartSession(_ context.Context, id string) (AgentSession, error) {
	a.started = append(a.started, id)
	return &stubAgentSession{}, nil
}

func (a *forkingAgent) StartForkedSession(_ context.Context, id string) (AgentSession, error) {
	a.forked = append(a.forked, id)
	return &stubAgentSession{}, nil
}

func TestForkFromAgentSessionStartsANewPendingFork(t *testing.T) {
	sm := NewSessionManager("")
	old := sm.GetOrCreateActive("chat")
	old.SetAgentSessionID("src", "stub")

	s := sm.ForkFromAgentSession("chat", "src", "stub", "summary")
	if s.ID == old.ID {
		t.Fatalf("fork must start a new conversation, got the existing session %s", s.ID)
	}
	if sm.ActiveSessionID("chat") != s.ID || s.GetAgentSessionID() != "src" || !s.IsForkPending() {
		t.Fatalf("unexpected fork session: active=%s agent=%s pending=%v", sm.ActiveSessionID("chat"), s.GetAgentSessionID(), s.IsForkPending())
	}

	// The pending flag clears once the agent reports the fork's own ID.
	s.SetAgentSessionID("forked", "stub")
	if s.IsForkPending() {
		t.Fatalf("fork flag should clear when the agent session ID changes")
	}
}

func TestStartOrForkAgentSession(t *testing.T) {
	e := newTestEngine()
	agent := &forkingAgent{}
	sm := NewSessionManager("")
	s := sm.ForkFromAgentSession("chat", "src", "stub", "")

	if _, err := e.startOrForkAgentSession(agent, s, "src"); err != nil {
		t.Fatal(err)
	}
	if len(agent.forked) != 1 || agent.forked[0] != "src" || len(agent.started) != 0 {
		t.Fatalf("pending fork should use StartForkedSession: forked=%v started=%v", agent.forked, agent.started)
	}

	s.SetAgentSessionID("forked", "stub")
	if _, err := e.startOrForkAgentSession(agent, s, "forked"); err != nil {
		t.Fatal(err)
	}
	if len(agent.started) != 1 || agent.started[0] != "forked" {
		t.Fatalf("after the fork, later starts resume normally: started=%v", agent.started)
	}

	// Agents without fork support resume in place.
	plain := &stubAgent{}
	if _, err := e.startOrForkAgentSession(plain, sm.ForkFromAgentSession("chat", "src", "stub", ""), "src"); err != nil {
		t.Fatal(err)
	}
}

type contextPlatform struct {
	stubPlatformEngine
	gotConversation string
}

func (p *contextPlatform) AgentContext(_ any, conversationID string) string {
	p.gotConversation = conversationID
	return "[group messages]"
}

func TestWithAgentContext(t *testing.T) {
	s := NewSessionManager("").GetOrCreateActive("chat")
	p := &contextPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}

	if got := withAgentContext(p, nil, s, "hi"); got != "[group messages]\nhi" || p.gotConversation != s.ID {
		t.Fatalf("got %q for conversation %q", got, p.gotConversation)
	}
	if got := withAgentContext(&stubPlatformEngine{n: "test"}, nil, s, "hi"); got != "hi" {
		t.Fatalf("platforms without the capability must not change the prompt: %q", got)
	}
}
