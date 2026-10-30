# 飞书群协作：创建会话后直接讨论

每个人在自己的电脑上运行 cc-connect，把自己的飞书机器人加入群。一台电脑只需配置一组共享 Agent（Codex、Claude Code），再把多个机器人加入 bot 列表。每种 Agent 的会话工作目录只配置一次，所有 bot 共用这组目录设置；每个 bot 在每个群里的会话独立。原生会话记录仍由对应 CLI 管理。

## 安装与配置

如果二进制已安装，直接打开配置向导：

```bash
cc-connect setup
```

向导随二进制分发，会优先复用已激活或其他已有的 Python 3.11+ Conda 环境。它会检查选定 Agent 的登录状态，引导继续完成配置。只有源码编译才需要 Go 1.25+，配置阶段不需要源码和 Go。

**安装不能止于版本检查。** 如果由 AI 助手协助，应逐项向用户展示问题、默认值和授权步骤，得到回答后继续；不得自行代填默认路径，也不得只以“机器人和后台尚未配置”结束任务。

向导依次完成：

1. 选择 `codex`、`cc` 或 `codex,cc`。
2. 分别询问 Codex 和 Claude Code 的会话工作目录，展示默认路径；回车接受，或输入自定义路径。目录不存在时会创建。已有配置也会再次询问，不会静默跳过。
3. 扫码新建飞书机器人，或输入已有机器人的 App ID 和 App Secret。
4. 检查权限和名字，保存到机器人列表。配置期间不需要预先加入群。
5. 询问启动方式。选择后台服务后，验证新的启动日志；未启动或启动未确认时明确标记，不报告安装配置完成。

首次配置时会显示：

```text
Codex 会话工作目录（回车使用默认，或输入自定义路径）
默认：~/.cc-connect-collab/workspaces/codex

Claude Code 会话工作目录（回车使用默认，或输入自定义路径）
默认：~/.cc-connect-collab/workspaces/cc
```

重新配置时展示当前目录作为默认值。只有用户明确传入 `--codex-dir` / `--cc-dir` 时，才使用指定路径而不询问。

## 再添加一个机器人

```bash
cc-connect setup --add-bot
```

只添加新机器人的 App ID / App Secret（也可以扫码新建），复用已经确认的 Agent 类型、目录及同一个后台服务。正常情况下不再编译、不要求新目录、不复制项目；保存后重启现有服务即可生效。旧版程序首次升级到机器人池时需要编译一次。

把机器人拉入群后，由主人发送 `/codex-create` 或 `/cc-create`。如需主动限制可响应的群，可以在向导中加 `--choose-chats`；更新已有 bot 时会保留其现有群范围。

运行 `cc-connect setup`（不带 `--add-bot`），会重新询问 Agent 和目录，并列出已有 bot，可选择修改或新增。旧版项目配置会迁移为机器人池，保留凭证和会话存储键。明确想在添加 bot 的同时重选目录，可以加 `--reconfigure-agents`。

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
[[agents]]
type = "codex"
[agents.options]
work_dir = "/path/chosen/by/user/codex"
backend = "app_server"
mode = "suggest"

[[agents]]
type = "claudecode"
[agents.options]
work_dir = "/path/chosen/by/user/claude"
mode = "dontAsk"
disallowed_tools = ["Bash", "Edit", "Write", "NotebookEdit"]
cmd = "claude --strict-mcp-config"

[[bots]]
name = "我的机器人 A"
app_id = "cli_example_a"
app_secret = "<your-secret-a>"

[[bots]]
name = "我的机器人 B"
app_id = "cli_example_b"
app_secret = "<your-secret-b>"
```

增加 bot 只增加一条 `[[bots]]`；不需要增加 `[[projects]]`，也不需要重复配置目录。后台会为各 bot 分别维护群绑定。旧版 `projects` 配置仍兼容；两种格式不能混写，由配置向导负责迁移。

## 运行边界

- 创建和审批仅机器人主人可操作，其他群成员可以自然语言交流。
- 默认 Claude Code 不可写文件或执行命令，不加载个人 MCP；Codex 使用只读文件沙箱，仍可能运行只读命令，个人 MCP 是否启用由 Codex 本机设置决定。
- 多机器人讨论需要各机器人开通接收其他机器人 @、读取群消息、读取群成员和发消息权限，安装向导会检查。
- 会话和模型在每个人自己的电脑上运行，电脑睡眠、关机或进程退出后机器人离线。
- 同一台电脑的多个 bot 共用一个后台服务，不需要再次安装服务或创建另一个 `--home`。
- 只有本机部署的会话可用；不是连接另一台电脑或云端会话的入口。

开发维护时仍可在 Conda 环境运行 `python tools/feishu-collab-setup.py`；普通用户使用 `cc-connect setup` 即可。
