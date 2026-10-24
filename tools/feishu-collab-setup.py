#!/usr/bin/env python3
"""飞书群协作机器人一键配置（cc-connect 群协作模式，说明见 docs/feishu-collab.md）。

在仓库根目录运行：
  python3 tools/feishu-collab-setup.py                        # 扫码新建机器人
  python3 tools/feishu-collab-setup.py --app cli_xxx:密钥     # 绑定已有机器人

依次做：编译本仓库的 cc-connect → 生成配置 → 建机器人 → 检查权限和名字 → 选讨论群 → 装后台服务。
可以重复运行：已有的配置和机器人会沿用，只补没做完的步骤；git pull 之后重跑就是更新。
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
OPEN_API = "https://open.feishu.cn"
PROJECT = "collab"
DEFAULT_NAME_SUFFIX = "的智能助手"  # 扫码建的机器人默认叫「<人名>的智能助手」

# 群协作要的权限。扫码建的机器人自带大部分，通常只缺前三个（都是免审权限，开通立即生效）。
REQUIRED_SCOPES = [
    "im:message.group_msg",  # 读群里全部消息：被 @ 时补上之前的讨论
    "im:message.group_msg.include_bot:read",  # 读其他机器人发的群消息
    "im:chat.members:read",  # 查群成员：回复里按名字 @ 人
    "im:message.group_at_msg.include_bot:readonly",  # 收到其他机器人 @ 自己的消息
    "im:message.group_at_msg:readonly",  # 收到群里 @ 自己的消息
    "im:message:readonly",  # 读消息内容
    "im:message:send_as_bot",  # 以机器人身份发消息
    "application:application:self_manage",  # 读应用信息：自动识别机器人主人
]

CONFIG_TEMPLATE = """\
# 飞书群协作机器人配置，由 tools/feishu-collab-setup.py 生成，说明见 docs/feishu-collab.md
language = "zh"
data_dir = @DATA_DIR@

[log]
level = "info"

# 群里只发最终回复，不刷思考过程和工具进度
[display]
thinking_messages = false
tool_messages = false
reply_footer = false
show_context_indicator = false

[stream_preview]
enabled = false

[[projects]]
name = "collab"
reset_on_idle_mins = 0  # 闲置多久都不自动换新会话

[projects.agent]
type = "claudecode"

[projects.agent.options]
work_dir = @WORK_DIR@  # Agent 读这个目录的代码；/list 列的是在这个目录里开过的 Claude Code 会话
# 默认只读讨论：群里谁都能 @ 它，所以不让它改文件、跑命令。
# dontAsk：没预先放行的工具一律拒绝，不在群里弹确认；disallowed_tools 优先于你 ~/.claude/settings.json 里的放行规则。
mode = "dontAsk"
disallowed_tools = ["Bash", "Edit", "Write", "NotebookEdit"]
# --strict-mcp-config：不加载你自己配的 MCP 连接器（飞书、数据库等），免得群里的人借机器人访问
cmd = "claude --strict-mcp-config"
# model = "sonnet"  # 不填就用你 Claude Code 的默认模型

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
collab = true                    # 群协作模式
share_session_in_channel = true  # 一个群共用一个会话
allow_from = "*"                 # 群里谁都能 @ 它对话；命令和审批只认主人
allow_chat = ""                  # 只在这些群里响应，脚本选群后填入
group_only = true                # 不理私聊
"""


class ApiError(Exception):
    pass


class OpenAPI:
    """用机器人身份（tenant_access_token）调飞书开放平台接口。"""

    def __init__(self, app_id, app_secret):
        self.app_id = app_id
        self.app_secret = app_secret
        self.token = ""

    def call(self, method, path, params=None, body=None, auth=True):
        url = OPEN_API + path + ("?" + urllib.parse.urlencode(params) if params else "")
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Content-Type", "application/json; charset=utf-8")
        if auth:
            req.add_header("Authorization", "Bearer " + self.tenant_token())
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read()  # 飞书出错时也回 JSON，code 和 msg 里有原因
        except urllib.error.URLError as e:
            raise ApiError("连不上飞书开放平台：%s" % e.reason)
        try:
            out = json.loads(raw)
        except ValueError:
            raise ApiError("飞书返回的不是 JSON：%r" % raw[:200])
        if out.get("code", 0) != 0:
            raise ApiError("%s（code %s）" % (out.get("msg", ""), out.get("code")))
        return out

    def get(self, path, params=None):
        return self.call("GET", path, params)

    def tenant_token(self):
        if not self.token:
            out = self.call("POST", "/open-apis/auth/v3/tenant_access_token/internal",
                            body={"app_id": self.app_id, "app_secret": self.app_secret}, auth=False)
            self.token = out["tenant_access_token"]
        return self.token


def say(msg=""):
    print(msg, flush=True)


def warn(msg):
    say("⚠️  " + msg)


def fail(msg):
    say("❌ " + msg)
    sys.exit(1)


def step(n, title):
    say("\n== %d. %s" % (n, title))


def ask(prompt, default=""):
    try:
        ans = input("%s%s：" % (prompt, " [%s]" % default if default else "")).strip()
    except EOFError:
        fail("没有输入了，退出")
    return ans or default


def confirm(prompt, default=True):
    ans = ask(prompt + ("（Y/n）" if default else "（y/N）")).lower()
    return default if not ans else ans in ("y", "yes", "是", "好")


def wait_enter(prompt):
    """等用户在网页上操作完；返回 False 表示跳过这一步。"""
    return ask(prompt + "按回车，跳过这一步输入 s").lower() != "s"


def toml_str(s):
    return json.dumps(s, ensure_ascii=False)


def read_option(cfg_path, key):
    m = re.search(r'^\s*%s\s*=\s*"([^"]*)"' % re.escape(key), cfg_path.read_text(encoding="utf-8"), re.M)
    return m.group(1) if m else ""


def check_tools():
    if not shutil.which("go"):
        fail("没找到 go。先装 Go 1.25 或更新的版本（macOS：brew install go）")
    claude = shutil.which("claude")
    if not claude:
        fail("没找到 claude 命令。先装 Claude Code 命令行版，装好后终端里能运行 claude")
    try:
        out = subprocess.run([claude, "auth", "status"], capture_output=True, text=True, timeout=30).stdout
        logged_in = json.loads(out).get("loggedIn")
    except Exception:
        logged_in = None
    if logged_in is False:
        warn("命令行版 claude 还没登录：另开一个终端运行 claude auth login（用 API Key 或公司网关的可以忽略）")
    say("✅ go 和 claude 都在")


def build(bin_path):
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    say("编译中，第一次要下载依赖，可能要几分钟…")
    cmd = ["go", "build", "-tags", "no_web", "-o", str(bin_path), "./cmd/cc-connect"]
    if subprocess.run(cmd, cwd=str(REPO)).returncode != 0:
        fail("编译失败，看上面的报错。Go 太旧会报 go.mod requires go >= 1.25；"
             "下载依赖慢可以先运行 go env -w GOPROXY=https://goproxy.cn,direct")
    say("✅ 已编译到 %s" % bin_path)


def pick_work_dir(given):
    while True:
        raw = given or ask("Agent 读哪个目录的代码？填路径，比如 ~/code/my-service")
        given = None
        path = Path(os.path.expanduser(raw)).resolve()
        if path.is_dir():
            return str(path)
        say("目录 %s 不存在，重新填" % path)


def write_config(cfg_path, home, work_dir):
    text = CONFIG_TEMPLATE.replace("@DATA_DIR@", toml_str(str(home / "data"))).replace("@WORK_DIR@", toml_str(work_dir))
    cfg_path.parent.mkdir(parents=True, exist_ok=True)
    cfg_path.write_text(text, encoding="utf-8")
    os.chmod(str(cfg_path), 0o600)  # 之后会写进机器人密钥


def setup_bot(bin_path, cfg_path, app):
    cmd = [str(bin_path), "feishu", "setup", "--config", str(cfg_path), "--project", PROJECT]
    if app:
        cmd += ["--app", app]
    else:
        say("马上会出一个二维码：用飞书手机 App 扫码，按提示确认创建机器人（10 分钟内有效）。")
    if subprocess.run(cmd).returncode != 0:
        fail("建机器人没成功，看上面的报错。也可以在开发者后台建好应用后用 --app app_id:app_secret 绑定")
    say("（上面关于 allow_from 和机器人菜单的提示可以忽略，群协作要的配置脚本已经写好了）")


def check_scopes(api):
    while True:
        try:
            app = api.get("/open-apis/application/v6/applications/" + api.app_id,
                          {"lang": "zh_cn", "user_id_type": "open_id"})["data"]["app"]
            have = {s.get("scope") for s in app.get("scopes") or []}
            missing = [s for s in REQUIRED_SCOPES if s not in have]
            if not missing and not (app.get("owner") or {}).get("owner_id"):
                warn("查不到机器人的主人，命令和审批会对所有人锁死。"
                     "在配置的 [projects.platforms.options] 里加 collab_owner = \"你的 open_id\"")
        except ApiError as e:
            warn("查不到应用的权限：%s。下面按全部所需权限给链接，已开通的会显示「已开通」" % e)
            missing = list(REQUIRED_SCOPES)
        if not missing:
            say("✅ 权限齐全")
            return
        say("还缺这些权限：\n  " + "\n  ".join(missing))
        say("打开下面的链接，逐个点「开通」（扫码建的机器人都是免审权限，开通立即生效，不用发版）：")
        say("  %s/app/%s/auth?q=%s&op_from=openapi&token_type=tenant" % (OPEN_API, api.app_id, ",".join(missing)))
        if not wait_enter("开通后"):
            warn("跳过了权限检查，缺权限时机器人读不到群消息或 @ 不了人")
            return


def check_name(api):
    while True:
        try:
            name = api.get("/open-apis/bot/v3/info").get("bot", {}).get("app_name", "")
        except ApiError as e:
            warn("查不到机器人名字：%s" % e)
            return ""
        if name and not name.endswith(DEFAULT_NAME_SUFFIX):
            say("✅ 机器人叫「%s」" % name)
            return name
        guess = name[:-len(DEFAULT_NAME_SUFFIX)] if name.endswith(DEFAULT_NAME_SUFFIX) else "你的名字"
        say("机器人现在叫「%s」。扫码建的机器人都叫「<人名>%s」，同一个群里容易重名，别的机器人也没法按名字 @ 它。" % (name, DEFAULT_NAME_SUFFIX))
        say("  1) 打开 %s/app/%s/baseinfo ，把「应用名称」改成好认的名字，比如「%s-Claude」" % (OPEN_API, api.app_id, guess))
        say("  2) 打开 %s/app/%s/version ，创建版本并发布（发布不用审核，点了就生效）" % (OPEN_API, api.app_id))
        if not wait_enter("改好并发布后"):
            return name


def pick_chats(api, bot_name, current):
    while True:
        try:
            items = api.get("/open-apis/im/v1/chats", {"page_size": 100}).get("data", {}).get("items") or []
        except ApiError as e:
            warn("查不到机器人所在的群：%s" % e)
            items = []
        if items:
            break
        say("机器人「%s」还不在任何群里。在讨论群里点「设置 → 群机器人 → 添加机器人」，搜它的名字加进来。" % bot_name)
        if not wait_enter("加好后"):
            return []
    chosen = [c for c in current.split(",") if c]
    for i, c in enumerate(items, 1):
        say("  %d. %s%s" % (i, c.get("name") or "未命名群", "（当前）" if c.get("chat_id") in chosen else ""))
    default = ",".join(str(i) for i, c in enumerate(items, 1) if c.get("chat_id") in chosen)
    if not default and len(items) == 1:
        default = "1"
    while True:
        ans = ask("在哪些群里协作？填序号，多个用逗号隔开", default)
        try:
            idx = [int(x) for x in re.split(r"[,，\s]+", ans) if x]
        except ValueError:
            idx = []
        if idx and all(1 <= i <= len(items) for i in idx):
            return [items[i - 1]["chat_id"] for i in idx]
        say("填列表里的序号，比如 1 或 1,3")


def set_allow_chat(cfg_path, chat_ids):
    text = cfg_path.read_text(encoding="utf-8")
    pattern = re.compile(r"^allow_chat\s*=.*$", re.M)
    value = ",".join(chat_ids)
    if len(pattern.findall(text)) != 1:
        warn('没能自动写入，请在 %s 的 [projects.platforms.options] 里手动加：allow_chat = "%s"' % (cfg_path, value))
        return
    text = pattern.sub(lambda _: 'allow_chat = "%s"  # 只在这些群里响应' % value, text)
    cfg_path.write_text(text, encoding="utf-8")
    say("✅ 已写入配置：只在选中的 %d 个群里响应" % len(chat_ids))


def start(bin_path, cfg_path, log_path, no_service):
    if no_service or not confirm("装成后台服务吗？开机自动运行，关掉终端也不停"):
        say("没装后台服务。要用的时候在普通终端里前台运行（关掉终端就停）：")
        say("  %s --config %s" % (bin_path, cfg_path))
        if os.environ.get("CLAUDECODE"):
            warn("别在 Claude Code 里跑这条：会继承它的环境变量，机器人里的 claude 会出错")
        return False
    shell_auth = [k for k in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL") if os.environ.get(k)]
    if shell_auth:
        warn("你的终端里设了 %s。后台服务读不到终端里 export 的变量，"
             "要写进 ~/.claude/settings.json 的 env，否则后台跑的 claude 会登录失败" % "、".join(shell_auth))
    cmd = [str(bin_path), "daemon", "install", "--config", str(cfg_path), "--log-file", str(log_path)]
    log_offset = log_path.stat().st_size if log_path.exists() else 0
    r = subprocess.run(cmd, capture_output=True, text=True)
    sys.stdout.write(r.stdout)
    sys.stderr.write(r.stderr)
    if r.returncode != 0:
        if "already installed" not in r.stderr:
            fail("装后台服务失败，看上面的报错")
        say("这台电脑已经装过 cc-connect 后台服务（一台电脑只能装一个），现在的状态：")
        subprocess.run([str(bin_path), "daemon", "status"])
        if not confirm("换成这份配置重装吗？会重启后台服务", default=False):
            return False
        if subprocess.run(cmd + ["--force"]).returncode != 0:
            fail("重装后台服务失败，看上面的报错")
    for _ in range(30):  # 等它启动完
        if log_path.exists():
            with open(str(log_path), "rb") as f:
                f.seek(log_offset if log_path.stat().st_size >= log_offset else 0)
                if b'msg="cc-connect is running"' in f.read():
                    say("✅ 后台服务已启动")
                    return True
        time.sleep(1)
    warn("30 秒内没在日志里看到启动成功，看看日志：tail -f %s" % log_path)
    return True


def main():
    ap = argparse.ArgumentParser(description="飞书群协作机器人一键配置，说明见 docs/feishu-collab.md")
    ap.add_argument("--home", default="~/.cc-connect-collab", help="程序、配置、日志放在哪（默认 ~/.cc-connect-collab）")
    ap.add_argument("--work-dir", help="Agent 读哪个目录的代码（第一次生成配置时用）")
    ap.add_argument("--app", help="绑定已有机器人：app_id:app_secret（不填就扫码新建）")
    ap.add_argument("--skip-build", action="store_true", help="已经编译过时跳过编译")
    ap.add_argument("--no-service", action="store_true", help="不装后台服务，只打印前台启动命令")
    args = ap.parse_args()

    home = Path(os.path.expanduser(args.home)).resolve()
    bin_path = home / "bin" / "cc-connect"
    cfg_path = home / "config.toml"
    log_path = home / "logs" / "cc-connect.log"

    step(1, "检查环境")
    check_tools()

    step(2, "编译 cc-connect")
    if args.skip_build and bin_path.exists():
        say("跳过编译，用已有的 %s" % bin_path)
    else:
        build(bin_path)

    step(3, "生成配置")
    if cfg_path.exists():
        say("沿用已有配置 %s（Agent 读的目录：%s）" % (cfg_path, read_option(cfg_path, "work_dir")))
    else:
        write_config(cfg_path, home, pick_work_dir(args.work_dir))
        say("✅ 已生成 %s" % cfg_path)

    step(4, "建飞书机器人")
    app_id = read_option(cfg_path, "app_id")
    if app_id and not args.app:
        say("沿用已配置的机器人 %s" % app_id)
    else:
        setup_bot(bin_path, cfg_path, args.app)
    app_id, app_secret = read_option(cfg_path, "app_id"), read_option(cfg_path, "app_secret")
    if not app_id or not app_secret:
        fail("配置里没找到机器人的 app_id 和 app_secret，建机器人那一步可能没成功")
    os.chmod(str(cfg_path), 0o600)
    api = OpenAPI(app_id, app_secret)
    try:
        api.tenant_token()
    except ApiError as e:
        fail("机器人凭证不能用：%s" % e)

    step(5, "检查权限")
    check_scopes(api)

    step(6, "检查机器人名字")
    name = check_name(api) or "你的机器人"

    step(7, "选讨论群")
    chat_ids = pick_chats(api, name, read_option(cfg_path, "allow_chat"))
    if chat_ids:
        set_allow_chat(cfg_path, chat_ids)
    else:
        warn("没选群：机器人会在它所在的所有群里响应。之后可以重跑脚本再选")

    step(8, "启动")
    started = start(bin_path, cfg_path, log_path, args.no_service)

    say("\n🎉 配置完成。到群里做这几件事：")
    say("  1. 登记定位：@%s /role 一句话说明它负责什么、找它问什么" % name)
    say("     每个人的机器人都要登记一次，别的机器人才认得它、才能按名字 @ 它")
    say("  2. （可选）接上你终端里的会话：@%s /list 看会话列表，再 @%s /fork 序号" % (name, name))
    say("     绑的是副本，原会话不受影响；不绑就从新会话开始")
    say("  3. 讨论：@ 任意机器人提问，它会先读完群里之前的消息再回答")
    say("\n配置：%s" % cfg_path)
    if started:
        say("日志：%s" % log_path)
        say("改了配置要重启：%s daemon restart" % bin_path)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        say("\n已取消")
        sys.exit(130)
