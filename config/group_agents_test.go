package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGroupAgentsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, second string
		bad          bool
	}{
		{"both", "claudecode", false}, {"duplicate", "codex", true}, {"unsupported", "other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "[[projects]]\nname='group'\n[[projects.agents]]\ntype='codex'\n[projects.agents.options]\nwork_dir='~/codex-work'\n[[projects.agents]]\ntype='" + tc.second + "'\n[projects.agents.options]\nwork_dir='/tmp/cc'\n[[projects.platforms]]\ntype='feishu'\n"
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.bad {
				if err == nil {
					t.Fatal("invalid profiles accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Projects[0].Agent.Type != "codex" || len(cfg.Projects[0].Agents) != 2 {
				t.Fatal("profiles not loaded")
			}
		})
	}
}
