"""离线验证安装配置，不能触发建群、发消息或读取真实凭证。"""
import importlib.util
from pathlib import Path
import tempfile
import tomllib
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("setup", Path(__file__).with_name("feishu-collab-setup.py"))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class SetupTest(unittest.TestCase):
    def test_multiple_agents_and_upgrade_preserve_bot(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = root / "config.toml"
            profiles = {"codex": str(root / "codex"), "claudecode": str(root / "cc")}
            setup.write_config(cfg, root, profiles)
            parsed = tomllib.loads(cfg.read_text())
            self.assertEqual([a["type"] for a in parsed["projects"][0]["agents"]], ["codex", "claudecode"])
            # 模拟旧单 Agent 配置，迁移只动 Agent 段。
            cfg.write_text('[[projects]]\nname="collab"\n[projects.agent]\ntype="claudecode"\n[projects.agent.options]\nwork_dir="/tmp/old"\n[[projects.platforms]]\ntype="feishu"\n[projects.platforms.options]\napp_secret="TEST_PLACEHOLDER"\nallow_chat="test-chat"\n')
            setup.replace_agent_sections(cfg, profiles)
            setup.replace_agent_sections(cfg, {"codex": str(root / "new")})
            project = tomllib.loads(cfg.read_text())["projects"][0]
            self.assertEqual(len(project["agents"]), 1)
            self.assertEqual(project["agents"][0]["options"]["work_dir"], str(root / "new"))
            self.assertEqual(project["platforms"][0]["options"], {"app_secret": "TEST_PLACEHOLDER", "allow_chat": "test-chat"})
            self.assertEqual(cfg.stat().st_mode & 0o777, 0o600)

    def test_selection_creates_separate_directories(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(setup, "agent_command", return_value="test-cli"):
            root = Path(temp)
            profiles = setup.pick_agents("codex,cc,claudecode", root, str(root / "codex"), str(root / "cc"))
            self.assertEqual(len(profiles), 2)
            self.assertTrue(all(Path(p).is_dir() for p in profiles.values()))


if __name__ == "__main__":
    unittest.main()
