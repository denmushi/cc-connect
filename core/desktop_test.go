package core

import (
	"context"
	"golang.org/x/term"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type desktopTestAgent struct {
	groupTestAgent
	fail bool
}

func (a *desktopTestAgent) NativeDesktopCommand(string) (*NativeDesktopCommand, error) {
	mode := "success"
	if a.fail {
		mode = "fail"
	}
	return &NativeDesktopCommand{Args: []string{os.Args[0], "-test.run=^TestDesktopLaunchProcess$"}, Env: []string{"CC_DESKTOP_LAUNCH_TEST=" + mode}}, nil
}
func TestDesktopLaunchProcess(t *testing.T) {
	switch os.Getenv("CC_DESKTOP_LAUNCH_TEST") {
	case "success":
		os.Exit(0)
	case "fail":
		os.Exit(1)
	case "tty":
		if !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func TestDesktopLaunchProvidesTTY(t *testing.T) {
	command := &NativeDesktopCommand{Args: []string{os.Args[0], "-test.run=^TestDesktopLaunchProcess$"}, Env: []string{"CC_DESKTOP_LAUNCH_TEST=tty"}, PTY: true}
	if err := command.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopHandoffPersistsAndBlocksGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	a := &desktopTestAgent{groupTestAgent: groupTestAgent{kind: "claudecode"}}
	e := NewEngine("bot", a, nil, "", LangChinese)
	defer e.Stop()
	if err := e.SetGroupAgents(map[string]Agent{"claudecode": a}, path); err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "feishu"}
	e.handleMessage(p, &Message{SessionKey: "group", ChatName: "测试群", Content: "/cc-create"})
	s := waitGroupTurn(t, e, "group")
	rows := e.DesktopSessions()
	if len(rows) != 1 || rows[0].Name != "测试群 · Claude Code" {
		t.Fatalf("rows=%v", rows)
	}
	gen, _ := s.TryLock()
	if err := e.DesktopSessionAction(context.Background(), rows[0], "open"); err == nil {
		t.Fatal("busy handoff accepted")
	}
	s.Unlock(gen)
	if err := e.DesktopSessionAction(context.Background(), rows[0], "open"); err != nil {
		t.Fatal(err)
	}
	if !s.IsDesktopPaused() {
		t.Fatal("handoff not paused")
	}
	restored := NewSessionManager(path + ".claudecode.sessions.json")
	if !restored.GetOrCreateActive("group").IsDesktopPaused() {
		t.Fatal("pause lost on restart")
	}
	e.handleMessage(p, &Message{SessionKey: "group", Content: "不应该发给模型"})
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "已暂停") {
		t.Fatal("missing handoff notice")
	}
	if err := e.DesktopSessionAction(context.Background(), rows[0], "resume"); err != nil {
		t.Fatal(err)
	}
	if s.IsDesktopPaused() || s.GetAgentSessionID() != rows[0].SessionID {
		t.Fatal("resume lost identity")
	}
	a.fail = true
	if err := e.DesktopSessionAction(context.Background(), rows[0], "open"); err == nil {
		t.Fatal("launch failure hidden")
	}
	if !s.IsDesktopPaused() {
		t.Fatal("failed launch reopened bridge without confirming desktop state")
	}
}

func TestDesktopHandoffRequiresDurablePause(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bindings.json")
	a := &desktopTestAgent{groupTestAgent: groupTestAgent{kind: "claudecode"}}
	e := NewEngine("bot", a, nil, "", LangChinese)
	defer e.Stop()
	if err := e.SetGroupAgents(map[string]Agent{"claudecode": a}, path); err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "feishu"}
	e.handleMessage(p, &Message{SessionKey: "group", Content: "/cc-create"})
	s := waitGroupTurn(t, e, "group")
	_, sm := e.groupAgents.context("group")
	sm.storePath = root // 目录不能被原子替换为文件，模拟持久化失败。
	if err := e.DesktopSessionAction(context.Background(), e.DesktopSessions()[0], "open"); err == nil {
		t.Fatal("unpersisted handoff accepted")
	}
	if s.IsDesktopPaused() {
		t.Fatal("failed save changed ownership")
	}
}
