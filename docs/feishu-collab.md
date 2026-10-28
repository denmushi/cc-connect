# 飞书群协作：创建会话后直接讨论

每个人在自己的电脑上运行 cc-connect，把自己的飞书机器人加入群。一台电脑上的同一个机器人可以配置 Codex、Claude Code，或两种都配置。每种 Agent 指定一个会话工作目录；原生会话记录仍由对应 CLI 管理。

## 安装与配置

准备 Go 1.25+、已安装并登录的 Codex 或 Claude Code、飞书账号。Python 脚本只有标准库依赖，在已激活的 Conda 环境里运行：

```bash
python tools/feishu-collab-setup.py
```

向导依次完成：

1. 选择 `codex`、`cc` 或 `codex,cc`。
2. 为每种 Agent 分别指定会话工作目录。目录不存在时会创建。
3. 扫码新建飞书机器人，或输入已有机器人的 App ID 和 App Secret。
4. 检查权限和名字，将机器人加入讨论群，再选择允许响应的群。
5. 启动 cc-connect，可以选择安装后台服务。

也可以预先指定 Agent 和目录，机器人设置仍由向导处理：

```bash
python tools/feishu-collab-setup.py --agents codex,cc --codex-dir ~/work/codex --cc-dir ~/work/claude
```

修改已有配置或从旧版升级时加 `--reconfigure-agents`。脚本只更新 Agent 段，保留机器人凭证、群范围和其他设置。普通重跑沿用已有配置。

文件默认保存在 `~/.cc-connect-collab/`：`config.toml` 是配置，`data/` 是绑定和会话状态，`logs/cc-connect.log` 是运行日志。配置含机器人密钥，不要分享。

## 群内使用

从飞书的 @ 候选列表选中自己的机器人，然后发送以下任意一个命令：

```text
@我的机器人 /codex-create
```

```text
@我的机器人 /cc-create
```

只有配置过的 Agent 可用。命令会在对应工作目录启动一个全新原生会话，收到创建成功回复后即可交流。每个 bot 在每个群里绑定一个当前会话：再执行创建命令会换到新会话，旧会话保留；不同群互不影响。正在回复时不会替换绑定，需等待本轮完成。程序重启后恢复已经保存的绑定。

接下来直接说需求和分工，例如：

> 你负责接口方案，请和 @另一位机器人 讨论前端需要哪些数据。结论明确后告诉我。

不需要登记定位、查询会话序号、填写会话 ID。这套入口只执行 `/codex-create` 和 `/cc-create`；`/role`、`/list`、`/switch`、`/fork`、`/new` 等旧命令不再执行。自然语言发给当前 Agent 理解，不会额外解析为桥接程序的管理指令。

机器人通过群成员接口识别同伴，补读群历史后发 Markdown 卡片，需要交接时转换成真正的 @。没有人插话时，机器人连续发言默认最多 6 条。再次创建会话后重新读取最近 200 条群消息。

## 配置结构

```toml
[[projects]]
name = "collab"
reset_on_idle_mins = 0

[[projects.agents]]
type = "codex"
[projects.agents.options]
work_dir = "/path/to/codex-work"
backend = "app_server"
mode = "suggest"

[[projects.agents]]
type = "claudecode"
[projects.agents.options]
work_dir = "/path/to/claude-work"
mode = "dontAsk"
disallowed_tools = ["Bash", "Edit", "Write", "NotebookEdit"]
cmd = "claude --strict-mcp-config"

[[projects.platforms]]
type = "feishu"
[projects.platforms.options]
app_id = "<your-app-id>"
app_secret = "<your-app-secret>"
allow_chat = "<your-chat-id>"
allow_from = "*"
```

配置 `projects.agents` 后自动启用飞书群协作、按群共享会话和仅响应群聊。单独使用旧的 `projects.agent` 保留原有入口。

## 运行边界

- 创建和审批仅机器人主人可操作，其他群成员可以自然语言交流。
- 默认 Claude Code 不可写文件或执行命令，不加载个人 MCP；Codex 使用只读文件沙箱，仍可能运行只读命令，个人 MCP 是否启用由 Codex 本机设置决定。
- 多机器人讨论需要各机器人开通接收其他机器人 @、读取群消息、读取群成员和发消息权限，安装向导会检查。
- 会话和模型在每个人自己的电脑上运行，电脑睡眠、关机或进程退出后机器人离线。
- 一台电脑测试多个机器人时，分别使用 `--home` 创建独立配置，并用 `--no-service` 前台启动；后台服务名目前固定，一台电脑只安装一个。
- 只有本机部署的会话可用；不是连接另一台电脑或云端会话的入口。
