"""Opt-in browser exercise for workspace drag/drop and saved evidence."""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
import uuid
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

from browser_smoke import PLAYWRIGHT_IMAGE, PLAYWRIGHT_VERSION
from harness import COMPOSE_DIR, ROOT, StackUnavailable, compose_argv, stack


SCRIPT = r'''
import base64, hashlib, json, os
from playwright.sync_api import sync_playwright

base = os.environ["NETBOX_URL"].rstrip("/")
name = os.environ["PILOT_NAME"]
zip_path = "/out/demo.zip"
demo = "/demo"
filenames = ["networks.csv", "accounts.json", "failures.csv", "run.json",
             "matrix.yaml", "ownership.yaml", "fixed.yaml", "approved-plan.json",
             "pilot-scope.json", "migration.yaml", "execution.json"]

with sync_playwright() as play:
    browser = play.chromium.launch()
    context = browser.new_context(http_credentials={
        "username": os.environ["NETBOX_UI_USER"],
        "password": os.environ["NETBOX_UI_PASSWORD"],
    }, accept_downloads=True)
    page = context.new_page()
    page.goto(base + "/plugins/platform-ipam/", wait_until="networkidle")
    if page.get_by_role("heading", name="Migration workspace").count() != 1:
        raise RuntimeError("workspace did not render through ui-proxy")
    page.get_by_text("Upload pilot evidence or run another report").click()

    # The browser validates a partial extracted-file drop before reporting.
    partial = [{"name": "networks.csv", "data": base64.b64encode(
        open(demo + "/networks.csv", "rb").read()).decode()}]
    page.locator("#pilot-dropzone").evaluate("""(zone, rows) => {
      const transfer = new DataTransfer();
      for (const row of rows) {
        const raw = atob(row.data);
        const bytes = Uint8Array.from(raw, ch => ch.charCodeAt(0));
        transfer.items.add(new File([bytes], row.name));
      }
      zone.dispatchEvent(new DragEvent('drop', {dataTransfer: transfer, bubbles: true, cancelable: true}));
    }""", partial)
    if "Still needed:" not in page.locator("#upload-status").inner_text():
        raise RuntimeError("partial drop did not list the required files")
    page.get_by_role("button", name="Show report").click()
    if not page.locator("#client-upload-error").is_visible():
        raise RuntimeError("incomplete upload did not show a client-side validation error")

    # Drop all extracted files, run the assessment and check the rendered report.
    extracted = [{"name": filename, "data": base64.b64encode(
        open(demo + "/" + filename, "rb").read()).decode()} for filename in filenames]
    page.locator("#pilot-dropzone").evaluate("""(zone, rows) => {
      const transfer = new DataTransfer();
      for (const row of rows) {
        const raw = atob(row.data);
        const bytes = Uint8Array.from(raw, ch => ch.charCodeAt(0));
        transfer.items.add(new File([bytes], row.name));
      }
      zone.dispatchEvent(new DragEvent('drop', {dataTransfer: transfer, bubbles: true, cancelable: true}));
    }""", extracted)
    if "Required files ready." not in page.locator("#upload-status").inner_text():
        raise RuntimeError("complete extracted-file drop was not recognized")
    page.get_by_role("button", name="Show report").click()
    page.wait_for_load_state("networkidle")
    if page.locator("#pilot-scope").count() != 1 or "100 accounts" not in page.locator("#pilot-scope").inner_text():
        server_error = page.locator("#server-upload-error").inner_text() if page.locator("#server-upload-error").count() else "none"
        client_error = page.locator("#client-upload-error").inner_text()
        raise RuntimeError(f"the extracted-file pilot report did not render at {page.url}: "
                           + server_error + " / " + client_error + " / "
                           + page.locator("#upload-status").inner_text() + " / "
                           + page.locator("body").inner_text()[-800:])

    # Drop the ZIP and save one uniquely named immutable snapshot.
    page.locator("#pilot-dropzone").evaluate("""(zone, row) => {
      const raw = atob(row.data);
      const bytes = Uint8Array.from(raw, ch => ch.charCodeAt(0));
      const transfer = new DataTransfer();
      transfer.items.add(new File([bytes], row.name, {type: 'application/zip'}));
      zone.dispatchEvent(new DragEvent('drop', {dataTransfer: transfer, bubbles: true, cancelable: true}));
    }""", {"name": "demo.zip", "data": base64.b64encode(open(zip_path, "rb").read()).decode()})
    if not page.locator("#upload-status").inner_text().startswith("ZIP ready:"):
        raise RuntimeError("ZIP drag/drop was not recognized")
    page.locator("#pilot-name").fill(name)
    page.locator("input[name=save_snapshot]").check()
    page.get_by_role("button", name="Show report").click()
    if name not in page.locator("#saved-pilots").inner_text():
        raise RuntimeError("saved pilot is not shown in history")
    page.locator("#saved-pilots a").filter(has_text=name).first.click()
    page.wait_for_load_state("networkidle")
    if "Viewing" not in page.locator("#saved-pilots").inner_text():
        raise RuntimeError("saved pilot could not be reopened")
    with page.expect_download() as pending:
        page.get_by_role("link", name="Download saved evidence JSON").click()
    download = pending.value
    download.save_as("/out/export.json")
    exported = json.load(open("/out/export.json", encoding="utf-8"))
    if exported.get("pilot_owner") != "corporate-network-pilot":
        raise RuntimeError("downloaded snapshot evidence does not match the saved report")
    expected_hash = hashlib.sha256((json.dumps(exported, sort_keys=True, indent=2) + "\n").encode()).hexdigest()
    if page.locator("#saved-pilots code").inner_text() != expected_hash:
        raise RuntimeError("reloaded and downloaded snapshot differs from its displayed immutable digest")
    print(json.dumps({"ok": True, "name": name, "extracted_drop": True,
                      "zip_drop": True, "snapshot_reload": True, "download": True}))
    browser.close()
'''


class WorkspaceBrowserSmokeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if os.environ.get("IPAM_E2E_BROWSER") != "1":
            raise unittest.SkipTest("set IPAM_E2E_BROWSER=1 to run the workspace browser smoke")
        if shutil.which("docker") is None:
            raise StackUnavailable("docker is required for the browser smoke test")
        cls.stack = stack()
        if not cls.stack.ui_user or not cls.stack.ui_password:
            raise StackUnavailable("NETBOX_UI_USER and NETBOX_UI_PASSWORD are required")

    def test_drop_review_save_reload_and_export(self):
        name = f"workspace-smoke-{uuid.uuid4().hex}"
        demo = ROOT / "examples/migration-workspace/HUNDRED_ACCOUNT_DEMO"
        workspace_overlay = COMPOSE_DIR / "compose.netbox-workspace.yaml"
        containers = subprocess.run(
            compose_argv("ps", "-q", "ui-proxy"), cwd=COMPOSE_DIR,
            capture_output=True, text=True, timeout=60, check=False,
        )
        self.assertEqual(containers.returncode, 0, containers.stderr)
        proxy_id = containers.stdout.strip().splitlines()
        if not proxy_id:
            self.skipTest("ui-proxy is not running")

        with tempfile.TemporaryDirectory(prefix="platform-ipam-workspace-browser-") as directory:
            temporary = Path(directory)
            (temporary / "check.py").write_text(SCRIPT, encoding="utf-8")
            with ZipFile(temporary / "demo.zip", "w", compression=ZIP_DEFLATED) as bundle:
                for source in demo.iterdir():
                    if source.is_file() and source.name in (
                        "networks.csv", "accounts.json", "failures.csv", "run.json",
                        "matrix.yaml", "ownership.yaml", "fixed.yaml", "approved-plan.json",
                        "pilot-scope.json", "migration.yaml", "execution.json",
                    ):
                        bundle.write(source, arcname=source.name)
            try:
                result = subprocess.run([
                    "docker", "run", "--rm", "--network", f"container:{proxy_id[0]}",
                    "-v", f"{temporary}:/out",
                    "-v", f"{demo}:/demo:ro",
                    "-e", "NETBOX_URL=http://127.0.0.1:8080",
                    "-e", f"NETBOX_UI_USER={self.stack.ui_user}",
                    "-e", f"NETBOX_UI_PASSWORD={self.stack.ui_password}",
                    "-e", f"PILOT_NAME={name}",
                    PLAYWRIGHT_IMAGE, "sh", "-c",
                    f"pip install --quiet --break-system-packages playwright=={PLAYWRIGHT_VERSION} "
                    "&& python /out/check.py",
                ], capture_output=True, text=True, timeout=900, check=False)
                output = result.stdout + result.stderr
                self.assertEqual(result.returncode, 0, output[-2500:])
                self.assertIn('"ok": true', output)
                self.assertTrue((temporary / "export.json").is_file())
            finally:
                cleanup = (
                    "from platform_ipam_workspace.models import MigrationPilot, PilotSnapshot; "
                    f"pilot = MigrationPilot.objects.filter(name={name!r}); "
                    "PilotSnapshot.objects.filter(pilot__in=pilot).delete(); pilot.delete()"
                )
                compose = ["docker", "compose", "--env-file", str(COMPOSE_DIR / ".env"),
                           "-f", str(COMPOSE_DIR / "compose.yaml"),
                           "-f", str(COMPOSE_DIR / "compose.netbox.yaml"),
                           "-f", str(workspace_overlay), "exec", "-T", "netbox",
                           "/opt/netbox/venv/bin/python", "manage.py", "shell", "-c", cleanup]
                removed = subprocess.run(compose, cwd=COMPOSE_DIR, capture_output=True,
                                         text=True, timeout=120, check=False)
                self.assertEqual(removed.returncode, 0,
                                 f"could not remove test-only pilot {name}: {removed.stderr[-500:]}")


if __name__ == "__main__":
    unittest.main()
