package config

import (
	"fmt"
	"regexp"
	"strings"
)

type BotConfig struct {
	Name      string `toml:"name,omitempty"`
	AppID     string `toml:"app_id"`
	AppSecret string `toml:"app_secret"`
	AllowChat string `toml:"allow_chat,omitempty"`
	Owner     string `toml:"owner,omitempty"`
	// 迁移旧配置时保留存储键，已有群会话不因改名或新增 bot 丢失。
	StateKey string         `toml:"state_key,omitempty"`
	Options  map[string]any `toml:"options,omitempty"`
}

func (b BotConfig) runtimeKey() string {
	if b.StateKey != "" {
		return b.StateKey
	}
	return "bot_" + b.AppID
}

// RuntimeProjects 仅在运行时展开；配置文件始终保留共享 agents + bots，绝不回写展开结果。
// 每个 bot 有独立 Engine/绑定存储，但使用同一组目录设置。
func (c *Config) RuntimeProjects() []ProjectConfig {
	if len(c.Bots) == 0 {
		return c.Projects
	}
	projects := make([]ProjectConfig, 0, len(c.Bots))
	for _, bot := range c.Bots {
		agents := make([]AgentConfig, 0, len(c.Agents))
		for _, a := range c.Agents {
			agents = append(agents, cloneAgentConfig(a))
		}
		opts := cloneAnyMap(bot.Options)
		opts["app_id"], opts["app_secret"] = bot.AppID, bot.AppSecret
		opts["allow_chat"], opts["allow_from"] = bot.AllowChat, "*"
		if bot.Owner != "" {
			opts["collab_owner"] = bot.Owner
		}
		zero := 0
		project := ProjectConfig{Name: bot.runtimeKey(), Agents: agents, ResetOnIdleMins: &zero, Platforms: []PlatformConfig{{Type: "feishu", Options: opts}}}
		if len(agents) > 0 {
			project.Agent = cloneAgentConfig(agents[0])
		}
		projects = append(projects, project)
	}
	return projects
}

var botPoolKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func (c *Config) validateBotPool(permissive bool) error {
	if len(c.Projects) > 0 {
		return fmt.Errorf("机器人池不能与 projects 混用；请运行配置向导迁移旧配置")
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("请先配置 agents，为 Codex 或 Claude Code 指定会话工作目录")
	}
	// 复用现有 Agent 校验，并将展开后的目录保存到共享配置。
	probe := Config{Projects: []ProjectConfig{{Name: "bot-pool-check", Agents: c.Agents}}}
	if err := probe.validateInternal(true); err != nil {
		return err
	}
	c.Agents = probe.Projects[0].Agents
	if len(c.Bots) == 0 && !permissive {
		return fmt.Errorf("机器人池为空，请先添加一个 bot")
	}
	seenIDs, seenKeys := map[string]bool{}, map[string]bool{}
	for _, bot := range c.Bots {
		if !strings.HasPrefix(bot.AppID, "cli_") || !botPoolKeyPattern.MatchString(bot.AppID) || strings.TrimSpace(bot.AppSecret) == "" {
			return fmt.Errorf("bot 需要有效的 app_id 和 app_secret")
		}
		if seenIDs[bot.AppID] {
			return fmt.Errorf("机器人池中 app_id 重复：%s", bot.AppID)
		}
		if !botPoolKeyPattern.MatchString(bot.runtimeKey()) || seenKeys[bot.runtimeKey()] {
			return fmt.Errorf("机器人存储键非法或重复")
		}
		seenIDs[bot.AppID], seenKeys[bot.runtimeKey()] = true, true
	}
	return nil
}

// SavePoolBot 仅新增或更新指定机器人，保留其他机器人的凭证、群范围与共享 Agent 设置。
func SavePoolBot(appID, secret, owner string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := LoadPermissive(ConfigPath)
	if err != nil {
		return err
	}
	if len(cfg.Agents) == 0 {
		return fmt.Errorf("请先通过配置向导设置共享 Agent 和目录")
	}
	// 校验用生效配置，写回用原始配置，避免把其他 bot 的环境变量凭证展开保存。
	raw, err := loadLocked()
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Bots {
		if cfg.Bots[i].AppID == appID {
			cfg.Bots[i].AppSecret = secret
			raw.Bots[i].AppSecret = secret
			if owner != "" {
				cfg.Bots[i].Owner = owner
				raw.Bots[i].Owner = owner
			}
			found = true
			break
		}
	}
	if !found {
		cfg.Bots = append(cfg.Bots, BotConfig{AppID: appID, AppSecret: secret, Owner: owner})
		raw.Bots = append(raw.Bots, BotConfig{AppID: appID, AppSecret: secret, Owner: owner})
	}
	if err := cfg.validateBotPool(true); err != nil {
		return err
	}
	return saveConfig(raw)
}
