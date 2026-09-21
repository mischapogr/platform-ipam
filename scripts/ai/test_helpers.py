"""Behavioral tests for agent helpers; no application or live-cloud execution."""

import contextlib
import copy
import importlib.machinery
import importlib.util
import io
import json
from pathlib import Path
import shutil
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
import checks


def load_script(name):
    path = Path(__file__).parent / name
    loader = importlib.machinery.SourceFileLoader(name.replace("-", "_"), str(path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


inspect = load_script("inspect-allocation")
launcher = load_script("codex")


def plan(old="orders-v1", new="orders-v2", unknown=False, actions=None):
    return {"format_version": "1.2", "planned_values": {}, "resource_changes": [{
        "mode": "managed", "type": "platformipam_allocation", "address": "platformipam_allocation.vpc",
        "change": {"actions": actions or ["delete", "create"],
                   "before": {"allocation_key": old}, "after": {"allocation_key": new},
                   "after_unknown": {"allocation_key": unknown}},
    }]}


class ReplacementTests(unittest.TestCase):
    def test_distinct_generation_in_either_order(self):
        for actions in (["delete", "create"], ["create", "delete"]):
            self.assertEqual(checks.replacement_errors(plan(actions=actions)), [])

    def test_same_unknown_missing_or_empty_key_is_rejected(self):
        for kwargs in ({"new": "orders-v1"}, {"unknown": True}, {"new": None}, {"new": ""}):
            with self.subTest(kwargs=kwargs):
                self.assertTrue(checks.replacement_errors(plan(**kwargs)))

    def test_incomplete_future_and_state_documents_cannot_pass(self):
        for document in ({"format_version": "1.0", "values": {}},
                         dict(plan(), complete=False), dict(plan(), errored=True),
                         dict(plan(), deferred_changes=[{}]), dict(plan(), format_version="2.0"),
                         dict(plan(), resource_changes=[{"type": "platformipam_allocation"}])):
            with self.subTest(document=document):
                with self.assertRaises(ValueError):
                    checks.replacement_errors(document)

    def test_metadata_update_does_not_require_new_key(self):
        self.assertEqual(checks.replacement_errors(plan(new="orders-v1", actions=["update"])), [])


class InspectionTests(unittest.TestCase):
    def test_empty_blockers_cannot_become_reuse_approval(self):
        response = inspect.summarize({"id": "alloc_01", "state": "QUARANTINED", "release_blockers": []}, "alloc_01")
        self.assertEqual(response["reuse_decision"], "NOT_EVALUATED")
        self.assertEqual(response["aws_coverage"], "NOT_PROVIDED_BY_ALLOCATION_READ")
        self.assertEqual(response["inventory_sync"], "UNKNOWN")

    def test_wrong_identity_and_pending_operation_are_rejected(self):
        for document in ({"id": "alloc_02", "state": "ACTIVE"},
                         {"id": "alloc_01", "status": "PENDING"}):
            with self.assertRaises(ValueError):
                inspect.summarize(document, "alloc_01")

    def test_local_http_is_explicit_and_external_http_never_allowed(self):
        for url in ("http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"):
            with self.assertRaises(ValueError):
                inspect.endpoint(url, False)
            self.assertEqual(inspect.endpoint(url, True), url)
        for url in ("http://ipam.example.com", "https://user:token@ipam.example.com", "https://ipam.example.com?token=x"):
            with self.assertRaises(ValueError):
                inspect.endpoint(url, True)

    def test_oversized_response_is_rejected(self):
        with self.assertRaises(ValueError):
            inspect.read_document(io.BytesIO(b" " * (inspect.MAX_BYTES + 1)))


class LauncherAndReportingTests(unittest.TestCase):
    def test_legacy_terraform_is_a_tooling_blocker(self):
        report = checks.Report("test")
        try:
            with patch.object(checks.shutil, "which", return_value="/bin/terraform"), \
                 patch.object(checks.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="Terraform v0.11.10\n")):
                self.assertFalse(checks.terraform_ready(report))
            self.assertEqual(report.checks[-1]["status"], "BLOCKED")
        finally:
            report.artifacts.rmdir()

    def test_missing_implementation_returns_blocked(self):
        with tempfile.TemporaryDirectory() as temporary:
            with patch.object(checks, "ROOT", Path(temporary)):
                report = checks.Report("test")
                try:
                    checks.compose(report, None)
                    with contextlib.redirect_stdout(io.StringIO()) as output:
                        result = report.finish()
                    self.assertEqual(result, 2)
                    self.assertEqual(json.loads(output.getvalue())["status"], "BLOCKED")
                finally:
                    report.artifacts.rmdir()

    def test_contract_checks_links_in_native_skill_location(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(checks, "ROOT", Path(temporary)):
            skill = Path(temporary) / ".agents/skills/test/SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text("[missing](../../../docs/missing.md)\n")
            report = checks.Report("test")
            try:
                checks.contract(report, None)
                self.assertEqual(report.checks[0]["status"], "FAILED")
                self.assertIn(".agents/skills/test/SKILL.md", Path(report.checks[0]["log"]).read_text())
                self.assertTrue(any(item["check"] == "openapi" and item["status"] == "BLOCKED"
                                    for item in report.checks))
            finally:
                shutil.rmtree(report.artifacts)


class NativeLauncherTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.patch_root = patch.object(launcher, "ROOT", self.root)
        self.patch_root.start()
        self.addCleanup(self.patch_root.stop)
        self.path = self.root / ".codex/config.toml"
        self.path.parent.mkdir()
        self.path.write_text('model = "existing-model"\n[mcp_servers.test]\nenabled = true\n')
        self.config = {"mcp_servers": {
            "aws-knowledge": {"enabled": True, "url": "https://knowledge-mcp.global.api.aws"},
            "terraform": {"enabled": True, "command": "docker", "args": ["pinned-image", "--toolsets=registry"]},
            "context7": {"enabled": True}, "unrelated": {"enabled": False},
        }}
        self.native = {"config": copy.deepcopy(self.config), "layers": [{
            "name": {"type": "project", "dotCodexFolder": str(self.path.parent)},
            "config": copy.deepcopy(self.config),
        }]}
        self.native["config"]["mcp_servers"]["context7"].update({
            "command": "inherited", "env": {"API_KEY": "inherited-secret"}})
        self.parsed = SimpleNamespace(returncode=0, stdout=json.dumps([
            {"name": name, "enabled": settings["enabled"]}
            for name, settings in self.config["mcp_servers"].items()
        ]))

    def invoke(self, *arguments):
        with patch.object(sys, "argv", ["codex", *arguments]), \
             contextlib.redirect_stdout(io.StringIO()) as out, \
             contextlib.redirect_stderr(io.StringIO()) as err:
            result = launcher.run()
        return result, out.getvalue(), err.getvalue()

    def inspect_config(self):
        with patch.object(launcher, "native_read", return_value=self.native) as native, \
             patch.object(launcher.subprocess, "run", return_value=self.parsed) as parsed, \
             contextlib.redirect_stdout(io.StringIO()) as out:
            result = launcher.check_config("/bin/codex-real", self.config)
        self.assertEqual(native.call_args.args[1], "config/read")
        self.assertEqual(parsed.call_args.args[0], ["/bin/codex-real", "-C", str(self.root), "mcp", "list", "--json"])
        self.assertNotIn("inherited-secret", out.getvalue())
        return result, json.loads(out.getvalue())

    def test_native_layer_and_inherited_transport_pass(self):
        result, report = self.inspect_config()
        self.assertEqual(result, 0)
        self.assertTrue(report["project_layer_loaded"])

    def test_matching_flags_without_active_project_layer_cannot_pass(self):
        for layers in ([], [dict(self.native["layers"][0], disabledReason="Untrusted project")]):
            with self.subTest(layers=layers):
                self.native["layers"] = layers
                result, report = self.inspect_config()
                self.assertEqual(result, 1)
                self.assertFalse(report["project_layer_loaded"])

    def test_changed_url_image_or_toolset_cannot_hide_behind_matching_flags(self):
        for name, key, bad in (("aws-knowledge", "url", "https://wrong.example"),
                               ("terraform", "args", ["latest", "--toolsets=all"]),
                               ("context7", "enabled", False)):
            with self.subTest(name=name, key=key):
                native = copy.deepcopy(self.native)
                self.native["config"]["mcp_servers"][name][key] = bad
                result, report = self.inspect_config()
                self.assertEqual(result, 1)
                self.assertIn(name, report["mismatches"])
                self.native = native

    def test_launcher_preserves_unrelated_settings_and_literal_cli_overrides(self):
        original = self.path.read_bytes()
        args = ["-c", "mcp_servers.playwright.enabled=true", "exec", "literal $(echo unsafe) `id` with spaces"]
        with patch.object(launcher.shutil, "which", return_value="/bin/codex-real"), \
             patch.object(launcher.os, "execv") as execute:
            self.invoke(*args)
        execute.assert_called_once_with("/bin/codex-real", ["/bin/codex-real", "-C", str(self.root), *args])
        self.assertEqual(self.path.read_bytes(), original)

    def test_missing_native_config_does_not_use_old_fallback(self):
        old = self.root / "config/ai/codex.toml"
        old.parent.mkdir(parents=True)
        self.path.rename(old)
        result, _, error = self.invoke("--print-config")
        self.assertEqual(result, 2)
        self.assertIn("Missing native project configuration", error)

    def test_diagnostics_reject_overrides(self):
        for diagnostic in launcher.DIAGNOSTICS:
            result, _, error = self.invoke(diagnostic, "-c", "mcp_servers.test.enabled=true")
            self.assertEqual(result, 2)
            self.assertIn("overrides are excluded", error)

    def test_print_config_omits_project_secrets_and_unrelated_settings(self):
        self.path.write_text('model = "private-model"\n[mcp_servers.test]\nenabled = true\n'
                             'url = "https://example.test?token=url-secret"\n'
                             '[mcp_servers.test.http_headers]\nAuthorization = "header-secret"\n')
        result, output, error = self.invoke("--print-config")
        self.assertEqual(result, 0)
        self.assertEqual(error, "")
        for secret in ("private-model", "url-secret", "header-secret"):
            self.assertNotIn(secret, output)

    def test_malformed_toml_and_native_failures_do_not_print_secrets(self):
        self.path.write_text('invalid "syntax-secret"')
        result, output, error = self.invoke("--print-config")
        self.assertEqual(result, 2)
        self.assertNotIn("syntax-secret", output + error)
        with patch.object(launcher, "main", side_effect=ValueError("inherited-secret")), \
             contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(launcher.run(), 2)
        self.assertNotIn("inherited-secret", err.getvalue())

    def test_mcp_parser_failure_is_blocked_and_redacted(self):
        with patch.object(launcher, "native_read", return_value=self.native), \
             patch.object(launcher.subprocess, "run", return_value=SimpleNamespace(
                 returncode=1, stdout="inherited-secret", stderr="inherited-secret")):
            with self.assertRaises(launcher.InspectionError) as error:
                launcher.check_config("/bin/codex-real", self.config)
        self.assertNotIn("inherited-secret", str(error.exception))

    def test_discovery_requires_unique_enabled_skills_at_native_repo_paths(self):
        skills = [{"name": name, "enabled": True, "scope": "repo",
                   "path": str(self.root / ".agents/skills" / name / "SKILL.md")}
                  for name in launcher.SKILLS]
        variants = [(skills, 0), (skills[:-1], 1), (skills + skills[:1], 1),
                    ([dict(skills[0], enabled=False), *skills[1:]], 1),
                    ([dict(skills[0], scope="user"), *skills[1:]], 1),
                    ([dict(skills[0], path="/wrong/SKILL.md"), *skills[1:]], 1)]
        for items, expected in variants:
            with self.subTest(items=items), patch.object(launcher, "native_read", return_value={
                "data": [{"cwd": str(self.root), "skills": items, "errors": []}]
            }), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(launcher.check_skills("/bin/codex-real"), expected)

    def test_fresh_rpc_handshake_ignores_notifications_and_redacts_errors(self):
        fake = self.root / "fake-codex"
        fake.write_text('#!' + sys.executable + '\n' + '''
import json, sys
assert sys.argv[1:] == ["-C", str(__import__("pathlib").Path(__file__).parent), "app-server"]
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    print(json.dumps({"method": "notification", "params": {}}), flush=True)
    response = {"id": request["id"], "result": {"ok": True}}
    if request["method"] == "bad":
        response = {"id": request["id"], "error": {"message": "inherited-secret"}}
    print(json.dumps(response), flush=True)
''')
        fake.chmod(0o700)
        self.assertEqual(launcher.native_read(str(fake), "config/read", {}), {"ok": True})
        with self.assertRaises(launcher.InspectionError) as error:
            launcher.native_read(str(fake), "bad", {})
        self.assertNotIn("inherited-secret", str(error.exception))


if __name__ == "__main__":
    unittest.main()
