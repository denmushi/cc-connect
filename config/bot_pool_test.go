package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const poolFixture = `language = "zh"
[[agents]]
type = "codex"
[agents.options]
work_dir = "/tmp/codex-work"
backend = "app_server"
[[agents]]
type = "claudecode"
[agents.options]
work_dir = "/tmp/claude-work"
[[bots]]
app_id = "cli_first"
app_secret = "TEST_PLACEHOLDER_A"
allow_chat = "test-group"
state_key = "collab"
[[bots]]
app_id = "cli_second"
app_secret = "TEST_PLACEHOLDER_B"
allow_chat = "test-group"
`

func TestBotPoolSharedProfilesIndependentRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(poolFixture), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	projects := cfg.RuntimeProjects()
	if len(cfg.Projects) != 0 || len(projects) != 2 {
		t.Fatal("runtime expansion leaked into user config")
	}
	if projects[0].Name != "collab" || projects[1].Name != "bot_cli_second" {
		t.Fatal("binding storage identities were not preserved")
	}
	for _, project := range projects {
		if len(project.Agents) != 2 || project.Agents[0].Options["work_dir"] != "/tmp/codex-work" {
			t.Fatal("missing shared agent configuration")
		}
	}
	projects[0].Agents[0].Options["work_dir"] = "/tmp/changed"
	if projects[1].Agents[0].Options["work_dir"] != "/tmp/codex-work" || cfg.Agents[0].Options["work_dir"] != "/tmp/codex-work" {
		t.Fatal("mutable agent options shared between bots")
	}
	cfg.Bots[0].Name = "renamed"
	if cfg.RuntimeProjects()[0].Name != "collab" {
		t.Fatal("renaming a bot changed its session namespace")
	}
}

func TestBotPoolSavePreservesOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("BOT_POOL_TEST_SECRET", "TEST_ENV_SECRET")
	fixture := strings.Replace(poolFixture, "TEST_PLACEHOLDER_A", "${BOT_POOL_TEST_SECRET}", 1)
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	old := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = old })
	if err := SavePoolBot("cli_third", "TEST_PLACEHOLDER_C", ""); err != nil {
		t.Fatal(err)
	}
	if err := SavePoolBot("cli_third", "TEST_PLACEHOLDER_UPDATED", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Bots) != 3 || len(cfg.Agents) != 2 || cfg.Bots[0].AllowChat != "test-group" || cfg.Bots[0].StateKey != "collab" {
		t.Fatal("adding a bot changed existing configuration")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "[[projects]]") {
		t.Fatal("saved expanded projects instead of the pool")
	}
	if !strings.Contains(string(raw), "${BOT_POOL_TEST_SECRET}") || strings.Contains(string(raw), "TEST_ENV_SECRET") {
		t.Fatal("another bot's credential placeholder was expanded")
	}
}

func TestBotPoolValidation(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"duplicate bot":     func(c *Config) { c.Bots[1].AppID = c.Bots[0].AppID },
		"duplicate storage": func(c *Config) { c.Bots[1].StateKey = c.Bots[0].StateKey },
		"missing directory": func(c *Config) { delete(c.Agents[0].Options, "work_dir") },
		"mixed legacy":      func(c *Config) { c.Projects = []ProjectConfig{{Name: "old"}} },
	} {
		t.Run(name, func(t *testing.T) {
			c := Config{Agents: []AgentConfig{{Type: "codex", Options: map[string]any{"work_dir": "/tmp/work"}}}, Bots: []BotConfig{{AppID: "cli_first", AppSecret: "TEST", StateKey: "first"}, {AppID: "cli_second", AppSecret: "TEST"}}}
			change(&c)
			if c.validate() == nil {
				t.Fatal("invalid pool accepted")
			}
		})
	}
}
