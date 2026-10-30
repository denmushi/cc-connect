#!/usr/bin/env python3
"""配置共享 Agent 和飞书机器人池（Python 3.11+，在 Conda 环境运行）。

首次配置/重新配置：python tools/feishu-collab-setup.py
追加机器人：python tools/feishu-collab-setup.py --add-bot
正常配置会询问各 Agent 的目录，显示默认路径；追加 bot 复用已确认的 Agent 配置。
"""

import argparse
import getpass
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

from collab_config import (configure_agents, default_config, migrate_pool, read_config,
                           upsert_bot, write_config as write_pool_config)

REPO = Path(__file__).resolve().parent.parent
OPEN_API = "https://open.feishu.cn"
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




def agent_command(kind):
    if kind == "claudecode":
        return shutil.which("claude")
    found = shutil.which("codex")
    if found:
        return found
    for app in ("ChatGPT.app", "Codex.app"):
        for rel in ("Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex", "Contents/Resources/codex"):
            path = Path("/Applications") / app / rel
            if path.is_file() and os.access(str(path), os.X_OK):
                return str(path)
    return None


def check_tools(require_go=True):
    if require_go and not shutil.which("go"):
        fail("请先安装 Go 1.25 或更新版本")
    if not any(agent_command(kind) for kind in ("codex", "claudecode")):
        fail("请先安装并登录 Codex 或 Claude Code，至少需要一种")


def check_agent_login(agents):
    for agent in agents:
        kind = agent["type"]
        binary = agent_command(kind)
        if not binary:
            fail("未找到 %s 命令，请安装后重新运行 cc-connect setup" % kind)
        status_args = ["auth", "status"] if kind == "claudecode" else ["login", "status"]
        login_command = "claude auth login" if kind == "claudecode" else "codex login"
        while True:
            try:
                result = subprocess.run([binary] + status_args, capture_output=True, text=True, timeout=20)
                logged_in = result.returncode == 0
                if kind == "claudecode":
                    logged_in = logged_in and json.loads(result.stdout).get("loggedIn") is True
            except (OSError, ValueError, subprocess.TimeoutExpired):
                logged_in = False
            if logged_in:
                say("✅ %s 的本机登录状态正常" % kind)
                break
            say("还未确认 %s 可用。请另开终端运行 %s 完成登录。" % (kind, login_command))
            answer = ask("完成后回车重试；已自行验证 API/网关可用输入 s；取消输入 q").lower()
            if answer == "s":
                say("按你的确认使用现有 API/网关配置")
                break
            if answer == "q":
                fail("Agent 登录尚未完成，完成后请重新运行 cc-connect setup")


def pick_agents(given, home, codex_dir=None, cc_dir=None, current=None):
    current = current or {}
    aliases = {"codex": "codex", "cc": "claudecode", "claudecode": "claudecode"}
    available = [k for k in ("codex", "cc") if agent_command(aliases[k])]
    while True:
        explicit = given is not None
        raw = given or ask("配置哪些 Agent？填 codex、cc，或 codex,cc", ",".join(available))
        given = None
        keys = list(dict.fromkeys(x.strip().lower() for x in raw.split(",") if x.strip()))
        if keys and all(k in aliases and agent_command(aliases[k]) for k in keys):
            break
        if explicit:
            fail("--agents 只能选择已安装的 codex、cc 或 codex,cc")
        say("请选择已经安装的 Agent：codex、cc，支持多选")
    profiles = {}
    for key in keys:
        kind = aliases[key]
        if kind in profiles:
            continue
        supplied = codex_dir if kind == "codex" else cc_dir
        default = current.get(kind) or str(home / "workspaces" / key)
        label = "Codex" if kind == "codex" else "Claude Code"
        say("%s 默认会话工作目录：%s" % (label, default))
        raw = supplied or ask("%s 会话工作目录（回车使用默认，或输入自定义路径）" % label, default)
        path = Path(os.path.expanduser(raw)).resolve()
        path.mkdir(parents=True, exist_ok=True)
        profiles[kind] = str(path)
        say("✅ %s 会话工作目录：%s" % (label, path))
    return profiles



def build(bin_path):
    bin_path.parent.mkdir(parents=True, exist_ok=True)
    say("编译中，第一次要下载依赖，可能要几分钟…")
    cmd = ["go", "build", "-tags", "no_web", "-o", str(bin_path), "./cmd/cc-connect"]
    if subprocess.run(cmd, cwd=str(REPO)).returncode != 0:
        fail("编译失败，看上面的报错。Go 太旧会报 go.mod requires go >= 1.25；"
             "下载依赖慢可以先运行 go env -w GOPROXY=https://goproxy.cn,direct")
    say("✅ 已编译到 %s" % bin_path)


def supports_pool(bin_path):
    result = subprocess.run([str(bin_path), "feishu", "setup", "--help"],
                            capture_output=True, text=True, timeout=10)
    return "-pool" in result.stdout + result.stderr


def choose_app(given):
    """问用户有没有现成的机器人：有就返回 app_id:app_secret 用来绑定，没有返回空串、走扫码新建。"""
    if given:
        return given
    if not confirm("已经有现成的飞书机器人吗？没有就扫码新建一个", default=False):
        return ""
    say("App ID 和 App Secret 在开发者后台应用的「凭证与基础信息」里：%s/app" % OPEN_API)
    say("现成的机器人要先在「事件与回调」里选长连接，订阅事件 im.message.receive_v1 和回调 card.action.trigger；"
        "也不能同时给别的程序用，否则消息会被分走")
    while True:
        app_id = ask("App ID（cli_ 开头）")
        if app_id.startswith("cli_"):
            break
        say("App ID 是 cli_ 开头的那串")
    prompt = "App Secret（输入时不显示）："
    secret = getpass.getpass(prompt) if sys.stdin.isatty() else ask(prompt.rstrip("："))
    if not secret.strip():
        fail("没填 App Secret")
    return "%s:%s" % (app_id, secret.strip())


def setup_bot(bin_path, cfg_path, app):
    cmd = [str(bin_path), "feishu", "setup", "--config", str(cfg_path), "--pool"]
    if app:
        cmd += ["--app", app]
    else:
        say("马上会出一个二维码：用飞书手机 App 扫码，按提示确认创建机器人（10 分钟内有效）。")
    if subprocess.run(cmd).returncode != 0:
        fail("建机器人没成功，看上面的报错。也可以在开发者后台建好应用后用 --app app_id:app_secret 绑定")



def check_scopes(api, configured_owner=""):
    while True:
        try:
            app = api.get("/open-apis/application/v6/applications/" + api.app_id,
                          {"lang": "zh_cn", "user_id_type": "open_id"})["data"]["app"]
            have = {s.get("scope") for s in app.get("scopes") or []}
            missing = [s for s in REQUIRED_SCOPES if s not in have]
            if not missing and not ((app.get("owner") or {}).get("owner_id") or configured_owner):
                say("权限已齐全，但还不能识别机器人主人。请在开发者后台确认应用所有者和发布状态。")
                if not wait_enter("确认后"):
                    fail("主人身份尚未确认，配置未完成；确认后请重新运行 cc-connect setup")
                continue
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
            fail("权限尚未确认，配置未完成；开通后请重新运行 cc-connect setup")


def check_name(api):
    while True:
        try:
            name = api.get("/open-apis/bot/v3/info").get("bot", {}).get("app_name", "")
        except ApiError as e:
            fail("机器人信息尚未验证，配置未完成：%s。请处理后重新运行 cc-connect setup" % e)
        if name and not name.endswith(DEFAULT_NAME_SUFFIX):
            say("✅ 机器人叫「%s」" % name)
            return name
        guess = name[:-len(DEFAULT_NAME_SUFFIX)] if name.endswith(DEFAULT_NAME_SUFFIX) else "你的名字"
        say("机器人现在叫「%s」。扫码建的机器人都叫「<人名>%s」，同一个群里容易重名，别的机器人也没法按名字 @ 它。" % (name, DEFAULT_NAME_SUFFIX))
        say("  1) 打开 %s/app/%s/baseinfo ，把「应用名称」改成好认的名字，比如「%s-Claude」" % (OPEN_API, api.app_id, guess))
        say("  2) 打开 %s/app/%s/version ，创建版本并发布（发布不用审核，点了就生效）" % (OPEN_API, api.app_id))
        if not wait_enter("改好并发布后"):
            fail("机器人名字尚未确认，配置未完成；发布后请重新运行 cc-connect setup")


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




def wait_for_service_ready(log_path, log_offset):
    for _ in range(30):
        if log_path.exists():
            with open(str(log_path), "rb") as stream:
                stream.seek(log_offset if log_path.stat().st_size >= log_offset else 0)
                if b'msg="cc-connect is running"' in stream.read():
                    say("✅ 后台服务已启动")
                    return True
        time.sleep(1)
    fail("配置已保存，但未确认后台启动成功。请查看日志 %s，处理后重新运行 cc-connect setup" % log_path)


def start(bin_path, cfg_path, log_path, no_service):
    if not no_service:
        meta_path = Path.home() / ".cc-connect" / "daemon.json"
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            meta = {}
        installed_config = Path(meta.get("work_dir", "/nonexistent")) / "config.toml"
        if installed_config.resolve() == cfg_path.resolve() and Path(meta.get("binary_path", "/nonexistent")).resolve() == bin_path.resolve():
            status = subprocess.run([str(bin_path), "daemon", "status"], capture_output=True, text=True)
            if status.returncode == 0 and "not installed" not in status.stdout.lower():
                if not confirm("重启现有后台服务，让新的机器人列表生效吗"):
                    return False
                log_offset = log_path.stat().st_size if log_path.exists() else 0
                if subprocess.run([str(bin_path), "daemon", "restart"]).returncode != 0:
                    fail("重启失败，请查看后台服务日志")
                return wait_for_service_ready(log_path, log_offset)
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
    return wait_for_service_ready(log_path, log_offset)


def select_bot(data, add=False, credentials=None):
    bots = data.get("bots", [])
    if credentials or add or not bots:
        return None
    say("已配置的机器人（共用上面的 Agent 和目录）：")
    for index, bot in enumerate(bots, 1):
        say("  %d. %s（%s）" % (index, bot.get("name") or "未命名机器人", bot["app_id"]))
    while True:
        answer = ask("选择要配置的机器人序号；输入 a 添加一个机器人", "1")
        if answer.lower() == "a":
            return None
        if answer.isdigit() and 1 <= int(answer) <= len(bots):
            return dict(bots[int(answer) - 1])
        say("请输入列表中的序号，或 a")


def prepare_agents(data, args, home):
    current = {a["type"]: a.get("options", {}).get("work_dir", "") for a in data.get("agents", [])}
    if args.add_bot and current and not args.reconfigure_agents:
        for kind, directory in current.items():
            say("复用 %s 会话工作目录：%s" % (kind, directory))
        return
    # 正常安装/重新配置始终询问目录，已有配置也不能静默跳过。
    profiles = pick_agents(args.agents, home, args.codex_dir, args.cc_dir or args.work_dir, current)
    configure_agents(data, profiles)


def main():
    ap = argparse.ArgumentParser(description="配置共享 Agent 和飞书机器人池")
    ap.add_argument("--home", default="~/.cc-connect-collab")
    ap.add_argument("--agents", help="codex、cc 或 codex,cc")
    ap.add_argument("--codex-dir", help="明确指定 Codex 会话工作目录；省略时会询问")
    ap.add_argument("--cc-dir", help="明确指定 Claude Code 会话工作目录；省略时会询问")
    ap.add_argument("--work-dir", help="兼容旧版，相当于 --cc-dir")
    ap.add_argument("--add-bot", action="store_true", help="向机器人池添加 bot，复用共享 Agent 和目录")
    ap.add_argument("--choose-chats", action="store_true", help="可选：限制本次配置的 bot 只响应选定群")
    ap.add_argument("--reconfigure-agents", action="store_true", help="添加 bot 时也重新选择 Agent 和目录")
    ap.add_argument("--app", help="已有机器人 app_id:app_secret；省略时会询问或扫码新建")
    ap.add_argument("--skip-build", action="store_true")
    ap.add_argument("--installed-binary", help=argparse.SUPPRESS)
    ap.add_argument("--no-service", action="store_true")
    args = ap.parse_args()
    home = Path(os.path.expanduser(args.home)).resolve()
    bin_path, cfg_path, log_path = home / "bin/cc-connect", home / "config.toml", home / "logs/cc-connect.log"
    if args.installed_binary:
        bin_path = Path(args.installed_binary).resolve()
        if not bin_path.is_file():
            fail("已安装的 cc-connect 程序不存在，请重新安装后再运行 setup")
    existing = read_config(cfg_path) if cfg_path.exists() else default_config(home)
    data = migrate_pool(existing)
    if existing.get("projects"):
        say("首次把旧配置转换成机器人池，请先确认所有 bot 共用的 Agent 目录")
        args.reconfigure_agents = True
    skip_build = bin_path.exists() and (args.installed_binary or args.skip_build or args.add_bot)
    if skip_build and not supports_pool(bin_path):
        if args.installed_binary or args.skip_build:
            fail("已安装的程序不支持机器人池，请去掉 --skip-build 更新程序")
        say("旧程序需要升级一次以支持机器人池；之后添加 bot 会复用已安装的程序")
        skip_build = False

    step(1, "选择 Agent 和会话目录")
    check_tools(require_go=not skip_build)
    prepare_agents(data, args, home)
    check_agent_login(data["agents"])

    step(2, "准备程序")
    if skip_build:
        say("使用已安装的 %s" % bin_path)
    else:
        build(bin_path)
    # 先确保程序支持新格式，再保存迁移结果；编译失败时保留可运行的旧配置。
    write_pool_config(cfg_path, data)

    step(3, "配置机器人")
    bot = select_bot(data, args.add_bot, args.app)
    if bot is None:
        app = choose_app(args.app)
        if app:
            app_id, secret = app.split(":", 1)
            api = OpenAPI(app_id, secret)
            api.tenant_token()  # 验证后才写入，避免错误凭证覆盖已有 bot。
            bot = dict(next((b for b in data.get("bots", []) if b["app_id"] == app_id), {}))
            bot.update(app_id=app_id, app_secret=secret)
        else:
            before = {b["app_id"] for b in data.get("bots", [])}
            setup_bot(bin_path, cfg_path, "")
            data = read_config(cfg_path)
            created = [b for b in data.get("bots", []) if b["app_id"] not in before]
            if len(created) != 1:
                fail("无法确认新建的机器人，请检查机器人列表；没有覆盖其他机器人")
            bot = created[0]
    api = OpenAPI(bot["app_id"], bot["app_secret"])
    api.tenant_token()

    step(4, "检查权限和名字")
    check_scopes(api, bot.get("owner", ""))
    bot["name"] = check_name(api) or bot.get("name") or "你的机器人"

    step(5, "保存机器人列表")
    if args.choose_chats:
        chats = pick_chats(api, bot["name"], bot.get("allow_chat", ""))
        if not chats:
            fail("没有选择群，暂不启用这个机器人")
        bot["allow_chat"] = ",".join(chats)
    else:
        bot.setdefault("allow_chat", "")
        say("机器人配置好后再拉入群即可；由主人在群里创建会话。已有群范围限制会保留。")
    upsert_bot(data, bot)
    write_pool_config(cfg_path, data)
    say("✅ 机器人池现有 %d 个 bot；全部复用同一组 Agent 配置" % len(data["bots"]))

    step(6, "启动或更新后台服务")
    started = start(bin_path, cfg_path, log_path, args.no_service)
    if started:
        say("✅ 配置已完成，后台服务已启动。把机器人拉入飞书群后，@ 它发送 /codex-create 或 /cc-create。")
    else:
        say("配置已保存，但后台服务尚未启动；请按上面给出的命令启动，之后再进行群内测试。")
    say("以后添加机器人：cc-connect setup --add-bot")
    say("配置：%s" % cfg_path)
    if started:
        say("日志：%s" % log_path)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, ApiError) as exc:
        fail(str(exc))
    except KeyboardInterrupt:
        say("\n已取消")
        sys.exit(130)
