"""发现 Agent 程序来源；检测结果不代表用户已经选择或授权使用。"""
import os
from pathlib import Path
import shutil
import sys


SOURCE_LABELS = {"cli": "独立 CLI", "desktop": "桌面版内置 CLI", "custom": "自定义程序路径"}
BINARY_NAMES = {"codex": "codex", "claudecode": "claude"}


def resolve_executable(value):
    value = os.path.expanduser(value)
    found = shutil.which(value)
    if not found:
        raise ValueError("程序不存在或不可执行：%s" % value)
    # 保存真实绝对路径，避免 daemon 的 PATH 或命令软链接改变后换到另一来源。
    return str(Path(found).resolve(strict=True))


def executable_source(path):
    parts = Path(path).parts
    if any(part.lower().endswith(".app") for part in parts):
        return "desktop"
    claude_cache = Path.home() / "Library/Application Support/Claude/claude-code"
    if Path(path).is_relative_to(claude_cache):
        return "desktop"
    return "cli"


def discover_executables(kind):
    candidates = {"cli": [], "desktop": []}

    def add(value):
        try:
            path = resolve_executable(str(value))
        except (ValueError, OSError, RuntimeError):
            return
        source = executable_source(path)
        if path not in candidates[source]:
            candidates[source].append(path)

    # 必须检查整个 PATH：前面的 codex 可能只是桌面内置程序的软链接。
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        if not os.path.isabs(directory):
            continue
        found = shutil.which(BINARY_NAMES[kind], path=directory)
        if found:
            add(found)
    if sys.platform == "darwin":
        if kind == "codex":
            for root in (Path("/Applications"), Path.home() / "Applications"):
                for app in ("ChatGPT.app", "Codex.app"):
                    for relative in ("Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
                                     "Contents/Resources/codex"):
                        add(root / app / relative)
        else:
            cache = Path.home() / "Library/Application Support/Claude/claude-code"
            for path in sorted(cache.glob("*/*/claude.app/Contents/MacOS/claude"), reverse=True):
                add(path)
    return candidates


def configured_argv(options):
    command = options.get("cmd")
    # 与 Go 的 ParseCmdOpts 保持一致；含空格的程序路径必须使用数组。
    if isinstance(command, str) and command.strip():
        return command.split()
    if isinstance(command, list) and all(isinstance(item, str) for item in command):
        parts = [item for item in command if item]
        if parts:
            return parts
    for key in ("cli_path", "command"):
        value = options.get(key)
        if isinstance(value, str) and value.strip():
            return value.split()
    return []
