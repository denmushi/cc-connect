package core

import "testing"

func TestHandlePendingPermission_CannotApproveLeavesPromptPending(t *testing.T) {
	e := newTestEngine()
	sessionKey := "feishu:oc_chat"
	pending := &pendingPermission{RequestID: "req-1", Resolved: make(chan struct{})}
	session := &recordingAgentSession{}
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = &interactiveState{agentSession: session, pending: pending}
	e.interactiveMu.Unlock()
	p := &stubPlatformEngine{n: "test"}

	if e.handlePendingPermission(p, &Message{SessionKey: sessionKey, ReplyCtx: "ctx", CannotApprove: true}, "allow", sessionKey) {
		t.Fatal("a sender who cannot approve must not resolve the prompt")
	}
	if session.calls != 0 {
		t.Fatalf("permission answered %d times by a sender who cannot approve", session.calls)
	}

	if !e.handlePendingPermission(p, &Message{SessionKey: sessionKey, ReplyCtx: "ctx"}, "allow", sessionKey) {
		t.Fatal("the owner should still be able to approve")
	}
	if session.calls != 1 || session.lastID != "req-1" {
		t.Fatalf("owner approval not delivered: calls=%d id=%q", session.calls, session.lastID)
	}
}

func TestHandleAsyncUserInput_CannotApproveIsNotAnAnswer(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	s := &asyncInputSession{events: make(chan Event, 16)}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
	state := &interactiveState{agentSession: s, platform: p}
	key := "plain:user"
	e.interactiveStates[key] = state
	e.showAsyncUserInput(state, p, "ctx", Event{RequestID: "q", Questions: []UserQuestion{{Question: "Your preference?"}}})

	guest := &Message{SessionKey: key, ReplyCtx: "ctx", CannotApprove: true}
	if content, handled := e.handleAsyncUserInput(p, guest, "use SQLite", key, e.sessions); handled || content != "use SQLite" {
		t.Fatalf("a sender who cannot approve answered the question: %q, %v", content, handled)
	}
	if _, handled := e.handleAsyncUserInput(p, guest, "askq:cQ:0:1", key, e.sessions); !handled {
		t.Fatal("an option click from a sender who cannot approve must be dropped")
	}

	// The question is still open for the owner.
	prompt, handled := e.handleAsyncUserInput(p, &Message{SessionKey: key, ReplyCtx: "ctx"}, "custom answer", key, e.sessions)
	if handled || prompt != "Your preference?: custom answer" {
		t.Fatalf("owner answer = %q, %v", prompt, handled)
	}
}
