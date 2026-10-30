package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ccconnect "github.com/chenhg5/cc-connect"
)

func TestSetupCommandAndEmbeddedResources(t *testing.T) {
	if topLevelCommandHandlers["setup"] == nil {
		t.Fatal("setup is not available after binary installation")
	}
	for _, name := range []string{"feishu-collab-setup.py", "collab_config.py"} {
		data, err := ccconnect.SetupAssets.ReadFile("tools/" + name)
		if err != nil || len(data) == 0 {
			t.Fatalf("missing embedded setup resource %s: %v", name, err)
		}
	}
	script, _ := ccconnect.SetupAssets.ReadFile("tools/feishu-collab-setup.py")
	if !strings.Contains(string(script), "--installed-binary") {
		t.Fatal("embedded wizard would rebuild from unavailable source")
	}
}

func TestSetupNeverPromptsDaemonOrAssumesNonInteractiveAnswers(t *testing.T) {
	for _, tc := range []struct {
		tty  bool
		log  string
		want bool
	}{
		{true, "", true}, {false, "", false}, {true, "/tmp/service.log", false}, {false, "/tmp/service.log", false},
	} {
		if got := canStartSetup(tc.tty, tc.log); got != tc.want {
			t.Fatalf("tty=%v daemon=%q: got %v", tc.tty, tc.log, got)
		}
	}
}

func TestSetupConfigIsFoundOnNextLaunch(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".cc-connect", "config.toml")
	collab := filepath.Join(home, ".cc-connect-collab", "config.toml")
	if got := userConfigPath(home); got != legacy {
		t.Fatalf("legacy default changed: %s", got)
	}
	if err := os.MkdirAll(filepath.Dir(collab), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collab, []byte("language='zh'"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := userConfigPath(home); got != collab {
		t.Fatalf("setup result not discovered: %s", got)
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("language='en'"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := userConfigPath(home); got != legacy {
		t.Fatalf("existing explicit legacy config lost priority: %s", got)
	}
}
