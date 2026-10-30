"""离线推演首次安装和追加 bot，不读取真实凭证、不创建群或发送消息。"""
import argparse
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import collab_config

spec = importlib.util.spec_from_file_location("setup", Path(__file__).with_name("feishu-collab-setup.py"))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class FakeAPI:
    def __init__(self, app_id, secret):
        self.app_id = app_id

    def tenant_token(self):
        return "TEST_TOKEN"


class SetupTest(unittest.TestCase):
    def test_install_custom_default_paths_then_add_second_bot(self):
        with tempfile.TemporaryDirectory() as temp:
            home = Path(temp).resolve()
            output = io.StringIO()
            def build(binary):
                binary.parent.mkdir(parents=True, exist_ok=True)
                binary.touch()
            with contextlib.ExitStack() as stack:
                stack.enter_context(patch.object(setup, "check_tools"))
                stack.enter_context(patch.object(setup, "check_agent_login"))
                stack.enter_context(patch.object(setup, "agent_command", return_value="test-cli"))
                build_mock = stack.enter_context(patch.object(setup, "build", side_effect=build))
                stack.enter_context(patch.object(setup, "supports_pool", return_value=True))
                stack.enter_context(patch.object(setup, "OpenAPI", FakeAPI))
                stack.enter_context(patch.object(setup, "check_scopes"))
                stack.enter_context(patch.object(setup, "check_name", side_effect=lambda api: api.app_id))
                pick_chats = stack.enter_context(patch.object(setup, "pick_chats", side_effect=AssertionError("adding a bot must not require joining a group first")))
                stack.enter_context(patch.object(setup, "start", return_value=False))
                stack.enter_context(contextlib.redirect_stdout(output))
                with patch.object(sys, "argv", ["setup", "--home", str(home), "--no-service"]), \
                     patch("builtins.input", side_effect=["codex,cc", str(home / "custom-codex"), ""]) as prompts, \
                     patch.object(setup, "choose_app", return_value="cli_first:TEST_PLACEHOLDER_A"):
                    setup.main()
                    self.assertEqual(prompts.call_count, 3)
                first = collab_config.read_config(home / "config.toml")
                self.assertNotIn("projects", first)
                self.assertEqual(first["agents"][0]["options"]["work_dir"], str(home / "custom-codex"))
                self.assertEqual(first["agents"][1]["options"]["work_dir"], str(home / "workspaces/cc"))
                with patch.object(sys, "argv", ["setup", "--home", str(home), "--add-bot", "--no-service"]), \
                     patch("builtins.input", side_effect=AssertionError("adding a bot must reuse Agent directories")), \
                     patch.object(setup, "choose_app", return_value="cli_second:TEST_PLACEHOLDER_B"):
                    setup.main()
                final = collab_config.read_config(home / "config.toml")
                self.assertEqual(final["agents"], first["agents"])
                self.assertEqual(final["bots"][0], first["bots"][0])
                self.assertEqual(len(final["bots"]), 2)
                self.assertEqual(build_mock.call_count, 1)
                pick_chats.assert_not_called()
                self.assertEqual((home / "config.toml").stat().st_mode & 0o777, 0o600)
            self.assertIn("Codex 默认会话工作目录", output.getvalue())
            self.assertIn("Claude Code 默认会话工作目录", output.getvalue())
            self.assertNotIn("TEST_PLACEHOLDER", output.getvalue())

    def test_existing_config_still_asks_for_both_paths(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(setup, "agent_command", return_value="test-cli"):
            home = Path(temp).resolve()
            data = collab_config.default_config(home)
            collab_config.configure_agents(data, {"codex": str(home / "old-codex"), "claudecode": str(home / "old-cc")})
            args = argparse.Namespace(add_bot=False, reconfigure_agents=False, agents="codex,cc", codex_dir=None, cc_dir=None, work_dir=None)
            with patch("builtins.input", side_effect=[str(home / "chosen-codex"), str(home / "chosen-cc")]) as prompts:
                setup.prepare_agents(data, args, home)
                self.assertEqual(prompts.call_count, 2)
            self.assertEqual(data["agents"][1]["options"]["work_dir"], str(home / "chosen-cc"))

    def test_legacy_migration_and_upsert_preserve_state(self):
        old = {"log": {"level": "info"}, "projects": [{"name": "collab", "agents": [{"type": "codex", "options": {"work_dir": "/tmp/test", "model": "test-model"}}], "platforms": [{"type": "feishu", "options": {"app_id": "cli_first", "app_secret": "TEST_PLACEHOLDER", "allow_chat": "test-group", "encrypt_key": "TEST_KEY"}}]}]}
        data = collab_config.migrate_pool(old)
        bot = copy.deepcopy(data["bots"][0])
        self.assertEqual(bot["state_key"], "collab")
        collab_config.upsert_bot(data, {"app_id": "cli_second", "app_secret": "TEST_SECOND"})
        collab_config.upsert_bot(data, {"app_id": "cli_second", "app_secret": "TEST_UPDATED"})
        self.assertEqual(len(data["bots"]), 2)
        self.assertEqual(data["bots"][0], bot)
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "config.toml"
            collab_config.write_config(path, data)
            self.assertEqual(collab_config.read_config(path), data)
            self.assertNotIn("[[projects]]", path.read_text())

    def test_existing_service_restarts_instead_of_installing(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            binary, cfg = root / "bin/cc-connect", root / "config.toml"
            meta = root / ".cc-connect/daemon.json"
            meta.parent.mkdir()
            meta.write_text(json.dumps({"work_dir": str(root), "binary_path": str(binary)}))
            result = argparse.Namespace(returncode=0, stdout="Status: Running", stderr="")
            with patch.object(Path, "home", return_value=root), patch.object(setup, "confirm", return_value=True), \
                 patch.object(setup, "wait_for_service_ready", return_value=True), \
                 patch.object(setup.subprocess, "run", return_value=result) as run:
                self.assertTrue(setup.start(binary, cfg, root / "log", False))
            self.assertEqual([call.args[0][1:] for call in run.call_args_list], [["daemon", "status"], ["daemon", "restart"]])

    def test_old_log_does_not_count_as_successful_start(self):
        with tempfile.TemporaryDirectory() as temp:
            log = Path(temp) / "service.log"
            old = 'msg="cc-connect is running"\n'
            log.write_text(old)
            with patch.object(setup.time, "sleep"), contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit):
                setup.wait_for_service_ready(log, len(old.encode()))
            with log.open("a") as out:
                out.write(old)
            self.assertTrue(setup.wait_for_service_ready(log, len(old.encode())))

    def test_unconfirmed_required_permissions_cannot_finish_setup(self):
        api = FakeAPI("cli_test", "TEST_PLACEHOLDER")
        api.get = lambda *args: {"data": {"app": {"scopes": [], "owner": {"owner_id": "test-owner"}}}}
        with patch.object(setup, "wait_enter", return_value=False), contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit):
            setup.check_scopes(api)

    def test_installed_binary_does_not_need_go_or_source_checkout(self):
        with tempfile.TemporaryDirectory() as temp:
            home = Path(temp).resolve()
            binary = home / "installed-cc-connect"
            binary.touch()
            args = ["setup", "--home", str(home), "--installed-binary", str(binary)]
            with patch.object(sys, "argv", args), patch.object(setup, "supports_pool", return_value=True), \
                 patch.object(setup, "check_tools") as tools, patch.object(setup, "build", side_effect=AssertionError("must not rebuild")), \
                 patch.object(setup, "prepare_agents", side_effect=RuntimeError("reached interactive configuration")):
                with self.assertRaisesRegex(RuntimeError, "interactive configuration"):
                    setup.main()
                tools.assert_called_once_with(require_go=False)

    def test_login_failure_keeps_user_in_configuration_step(self):
        failed = argparse.Namespace(returncode=1, stdout="", stderr="")
        ready = argparse.Namespace(returncode=0, stdout='{"loggedIn": true}', stderr="")
        with patch.object(setup, "agent_command", return_value="test-cli"), \
             patch.object(setup.subprocess, "run", side_effect=[failed, ready]) as commands, \
             patch.object(setup, "ask", return_value="") as prompt, contextlib.redirect_stdout(io.StringIO()):
            setup.check_agent_login([{"type": "claudecode"}])
        self.assertEqual(commands.call_count, 2)
        prompt.assert_called_once()

    def test_declined_login_is_not_reported_as_completed(self):
        failed = argparse.Namespace(returncode=1, stdout="", stderr="")
        with patch.object(setup, "agent_command", return_value="test-cli"), \
             patch.object(setup.subprocess, "run", return_value=failed), patch.object(setup, "ask", return_value="q"), \
             contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit):
            setup.check_agent_login([{"type": "codex"}])


if __name__ == "__main__":
    unittest.main()
