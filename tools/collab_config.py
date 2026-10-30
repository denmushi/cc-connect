"""机器人池配置读写与旧配置迁移，只用 Python 标准库。"""
import copy
import datetime
import json
import os
from pathlib import Path
import re
import tempfile
try:
    import tomllib
except ModuleNotFoundError:
    raise SystemExit("请使用 Python 3.11 或更新版本的 Conda 环境运行配置向导")


def read_config(path):
    return tomllib.loads(Path(path).read_text(encoding="utf-8"))


def migrate_pool(data):
    """共享 Agent 配置；迁移保留每个旧机器人的存储键和其他配置字段。"""
    data = copy.deepcopy(data)
    projects = data.pop("projects", [])
    if not projects:
        data.setdefault("agents", [])
        data.setdefault("bots", [])
        return data
    if data.get("agents") or data.get("bots"):
        raise ValueError("配置同时包含机器人池和旧 projects，请先整理成一种格式")
    agents, bots = {}, []
    for project in projects:
        profiles = project.get("agents") or [project.get("agent", {})]
        for profile in profiles:
            if profile.get("type") not in ("codex", "claudecode"):
                raise ValueError("只能迁移 Codex/Claude Code 的群协作配置")
            agents.setdefault(profile["type"], profile)
        platforms = project.get("platforms", [])
        if len(platforms) != 1 or platforms[0].get("type") != "feishu":
            raise ValueError("旧配置需每个项目对应一个飞书 bot，才能保留原有会话绑定")
        options = copy.deepcopy(platforms[0].get("options", {}))
        bot = {"state_key": project["name"]}
        for key in ("app_id", "app_secret", "allow_chat"):
            bot[key] = options.pop(key, "")
        if options.get("collab_owner"):
            bot["owner"] = options.pop("collab_owner")
        if options:
            bot["options"] = options
        if bot["app_id"]:
            bots.append(bot)
    data["agents"], data["bots"] = list(agents.values()), bots
    return data


def default_config(home):
    return {"language": "zh", "data_dir": str(home / "data"),
            "log": {"level": "info"},
            "display": {"thinking_messages": False, "tool_messages": False,
                        "reply_footer": False, "show_context_indicator": False},
            "stream_preview": {"enabled": False}, "agents": [], "bots": []}


def configure_agents(data, directories):
    previous = {a["type"]: a for a in data.get("agents", [])}
    agents = []
    for kind, directory in directories.items():
        if kind in previous:
            agent = copy.deepcopy(previous[kind])
        elif kind == "codex":
            agent = {"type": kind, "options": {"backend": "app_server", "mode": "suggest"}}
        else:
            agent = {"type": kind, "options": {"mode": "dontAsk",
                     "disallowed_tools": ["Bash", "Edit", "Write", "NotebookEdit"],
                     "cmd": "claude --strict-mcp-config"}}
        agent.setdefault("options", {})["work_dir"] = directory
        agents.append(agent)
    data["agents"] = agents


def upsert_bot(data, bot):
    for index, existing in enumerate(data.setdefault("bots", [])):
        if existing["app_id"] == bot["app_id"]:
            data["bots"][index] = {**existing, **bot}
            return
    data["bots"].append(bot)


def dumps(data):
    """保留嵌套的模型/权限设置，不能用正则误改另一个 bot 的凭证。"""
    lines = []
    def key(value):
        return value if re.fullmatch(r"[A-Za-z0-9_-]+", value) else json.dumps(value, ensure_ascii=False)
    def scalar(value):
        if isinstance(value, str):
            return json.dumps(value, ensure_ascii=False)
        if isinstance(value, bool):
            return "true" if value else "false"
        if isinstance(value, (int, float)):
            return str(value).lower()
        if isinstance(value, (datetime.datetime, datetime.date, datetime.time)):
            return value.isoformat()
        if isinstance(value, list) and not any(isinstance(item, dict) for item in value):
            return "[" + ", ".join(scalar(item) for item in value) + "]"
        raise ValueError("配置包含无法安全写入的字段类型：%s" % type(value).__name__)
    def table(values, prefix=(), heading=None):
        if heading:
            lines.extend(["", heading])
        children = []
        for name, value in values.items():
            if isinstance(value, dict) or isinstance(value, list) and value and isinstance(value[0], dict):
                children.append((name, value))
            else:
                lines.append(key(name) + " = " + scalar(value))
        for name, value in children:
            parts = prefix + (name,)
            section = ".".join(key(part) for part in parts)
            if isinstance(value, dict):
                table(value, parts, "[" + section + "]")
            else:
                for item in value:
                    table(item, parts, "[[" + section + "]]")
    table(data)
    rendered = "\n".join(lines).strip() + "\n"
    tomllib.loads(rendered)  # 写入前先解析校验，原文件不会因格式错误被覆盖。
    return rendered


def write_config(path, data):
    rendered = dumps(data)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=path.parent, delete=False) as out:
            temporary = Path(out.name)
            os.chmod(temporary, 0o600)
            out.write(rendered)
        os.replace(temporary, path)
    finally:
        if temporary and temporary.exists():
            temporary.unlink()
