package core

import (
	"reflect"
	"testing"
)

func TestSessionAgentTypeRepairPreservesNativeIdentity(t *testing.T) {
	s := &Session{AgentSessionID: "native-id", AgentType: "codex", Name: "讨论",
		ContextScope: "claudecode", ForkPending: true, PastAgentSessionIDs: []string{"older-id"}}
	if !s.updateAgentSessionID("native-id", "claudecode") {
		t.Fatal("wrong type was not repaired")
	}
	if s.AgentSessionID != "native-id" || s.AgentType != "claudecode" || s.Name != "讨论" || s.ContextScope != "claudecode" || !s.ForkPending || !reflect.DeepEqual(s.PastAgentSessionIDs, []string{"older-id"}) {
		t.Fatalf("metadata repair changed native identity: %+v", s)
	}
	if s.updateAgentSessionID("native-id", "claudecode") {
		t.Fatal("unchanged metadata should not require a write")
	}
	if s.updateAgentSessionID(ContinueSession, "codex") || s.AgentType != "claudecode" {
		t.Fatal("continue sentinel changed metadata")
	}
	if !s.updateAgentSessionID("new-native-id", "claudecode") || s.ForkPending || !reflect.DeepEqual(s.PastAgentSessionIDs, []string{"older-id", "native-id"}) {
		t.Fatalf("native ID replacement no longer tracks history: %+v", s)
	}
}
