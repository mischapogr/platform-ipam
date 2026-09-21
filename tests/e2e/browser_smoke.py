"""Opt-in browser smoke test for the NetBox operator UI (ADR 0002).

The default suite asserts the operator surface through NetBox's REST API,
which is deterministic and needs no browser. That cannot catch a purely visual
regression -- a page that 500s, a custom-field column that stops rendering --
so this target exists to prove that an allocation is actually visible to a
person.

It is deliberately not discovered by `run-e2e.sh`: the filename does not match
`test_e2e_*.py`, and the test skips unless IPAM_E2E_BROWSER=1. Run it with:

    IPAM_E2E_BROWSER=1 python3 tests/e2e/browser_smoke.py

Playwright runs from a pinned image, so no browser install is needed on the
host. The test skips, rather than fails, when that image is unavailable.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from harness import COMPOSE_DIR, ROOT, StackUnavailable, compose_argv, run_key, stack  # noqa: E402

PLAYWRIGHT_IMAGE = "mcr.microsoft.com/playwright/python:v1.56.0-noble"
PLAYWRIGHT_VERSION = "1.56.0"
# The image ships the browsers under /ms-playwright but not the Python
# package, so it is installed at run time and pointed at those browsers
# rather than downloading a second copy.
INSTALL_AND_RUN = (
    f"pip install --quiet --break-system-packages playwright=={PLAYWRIGHT_VERSION} "
    "&& python /check.py"
)
SCRIPT = r"""
import json, os, sys
from playwright.sync_api import sync_playwright

base = os.environ["NETBOX_URL"].rstrip("/")
cidr = os.environ["EXPECT_CIDR"]
key = os.environ["EXPECT_KEY"]
prefix_id = os.environ["EXPECT_PREFIX_ID"]
ui_user = os.environ["NETBOX_UI_USER"]
ui_password = os.environ["NETBOX_UI_PASSWORD"]

with sync_playwright() as play:
    browser = play.chromium.launch()
    # ui-proxy (package A3) is the only published path to the NetBox UI, and
    # in `basic` mode it authenticates with HTTP Basic, not NetBox's own
    # login form: there is no form to fill in, and the remote user this
    # reaches is the view-only `platform-operators` member the proxy's
    # credential maps to, not the break-glass superuser.
    context = browser.new_context(http_credentials={"username": ui_user, "password": ui_password})
    page = context.new_page()

    # The allocation must be findable the way an operator would find it.
    page.goto(f"{base}/ipam/prefixes/?q={cidr}", wait_until="networkidle")
    if "/login" in page.url:
        print(json.dumps({"ok": False, "reason": "ui-proxy did not authenticate the request"}))
        sys.exit(1)
    if cidr not in page.content():
        print(json.dumps({"ok": False, "reason": f"{cidr} is not listed in the prefix view"}))
        sys.exit(1)

    # Navigate to the detail page by id rather than by clicking link text,
    # which depends on NetBox's table markup.
    page.goto(f"{base}/ipam/prefixes/{prefix_id}/", wait_until="networkidle")
    detail = page.content()
    missing = [label for label in (cidr, key, "RESERVED") if label not in detail]
    print(json.dumps({"ok": not missing, "missing": missing, "url": page.url,
                      "title": page.title()}))
    browser.close()
    sys.exit(1 if missing else 0)
"""


class NetBoxUISmokeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if os.environ.get("IPAM_E2E_BROWSER") != "1":
            raise unittest.SkipTest("set IPAM_E2E_BROWSER=1 to run the browser smoke test")
        if shutil.which("docker") is None:
            raise StackUnavailable("docker is required for the browser smoke test")
        cls.stack = stack()

    def test_an_allocation_is_visible_in_the_netbox_ui(self):
        allocation = self.stack.reserve(run_key("ui-smoke"), 22)
        env = self.stack.env

        script = ROOT / "tests/e2e/.workspace/browser_check.py"
        script.parent.mkdir(parents=True, exist_ok=True)
        script.write_text(SCRIPT, encoding="utf-8")

        row = self.stack.netbox_prefixes().get(allocation["cidr"])
        self.assertIsNotNone(row, f"{allocation['cidr']} is missing from NetBox")
        prefix_id = row["id"]

        containers = subprocess.run(
            compose_argv("ps", "-q", "ui-proxy"), cwd=COMPOSE_DIR,
            capture_output=True, text=True, timeout=60,
        ).stdout.strip().splitlines()
        if not containers:
            self.skipTest("the ui-proxy container is not running")

        result = subprocess.run([
            "docker", "run", "--rm",
            # Join ui-proxy's network namespace -- package A3 made it the
            # only published path to the NetBox UI, so this is now the
            # namespace a browser actually needs, not NetBox's own.
            "--network", f"container:{containers[0]}",
            "-v", f"{script}:/check.py:ro",
            "-e", "NETBOX_URL=http://127.0.0.1:8080",
            "-e", f"NETBOX_UI_USER={env.get('NETBOX_UI_USER', '')}",
            "-e", f"NETBOX_UI_PASSWORD={env.get('NETBOX_UI_PASSWORD', '')}",
            "-e", f"EXPECT_CIDR={allocation['cidr']}",
            "-e", f"EXPECT_KEY={allocation['allocation_key']}",
            "-e", f"EXPECT_PREFIX_ID={prefix_id}",
            "-e", "PLAYWRIGHT_BROWSERS_PATH=/ms-playwright",
            PLAYWRIGHT_IMAGE, "sh", "-c", INSTALL_AND_RUN,
        ], capture_output=True, text=True, timeout=900)

        combined = result.stdout + result.stderr
        for unavailable in ("manifest unknown", "no matching manifest", "pull access denied"):
            if unavailable in combined:
                self.skipTest(f"{PLAYWRIGHT_IMAGE} is unavailable")
        self.assertEqual(result.returncode, 0,
                         f"NetBox UI smoke failed:\n{combined[-1500:]}")


if __name__ == "__main__":
    unittest.main()
