"""End-to-end coverage of the Terraform surface (ADR 0002, ADR 0004).

The provider is unpublished and the host's Terraform version is not a
prerequisite, so this builds the provider from the working tree with the
pinned Go image and runs Terraform from a pinned image under a `dev_overrides`
CLI configuration.

Terraform joins the API container's network namespace, which is what lets the
provider's loopback-HTTP exemption apply: `127.0.0.1:8080` inside that
namespace is the platform API, so no plaintext credential crosses a network.

The suite skips rather than fails when Docker cannot build or run these
images, so a machine without them reports "not covered" instead of a false
defect.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import unittest

from harness import COMPOSE_DIR, ROOT, StackUnavailable, compose_argv, run_key, stack

GO_IMAGE = "golang:1.26.9-bookworm"
TERRAFORM_IMAGE = "hashicorp/terraform:1.14"
PROVIDER_SOURCE = "registry.example.com/platform/platformipam"
WORKSPACE = ROOT / "tests/e2e/.workspace"

MAIN_TF = """
terraform {
  required_providers {
    platformipam = {
      source = "%(source)s"
    }
  }
}

provider "platformipam" {
  endpoint = "http://127.0.0.1:8080"
}

variable "allocation_key" { type = string }

resource "platformipam_allocation" "vpc" {
  allocation_key = var.allocation_key
  scope          = "vpc"
  environment    = "development"
  region         = "eu-central-1"
  account_id     = "000000000000"
  prefix_length  = 22
  description    = "terraform end-to-end"
}

output "cidr" { value = platformipam_allocation.vpc.cidr }
output "allocation_id" { value = platformipam_allocation.vpc.id }
"""


def docker(*args: str, timeout: int = 900) -> subprocess.CompletedProcess:
    return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout)


class TerraformE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        if shutil.which("docker") is None:
            raise StackUnavailable("docker is required for the Terraform surface")
        cls.api_container = cls._api_container()
        cls.workspace = WORKSPACE
        cls._prepare_workspace()

    @classmethod
    def _api_container(cls) -> str:
        result = subprocess.run(compose_argv("ps", "-q", "api"), cwd=COMPOSE_DIR,
                                capture_output=True, text=True, timeout=60)
        containers = result.stdout.strip().splitlines()
        if not containers:
            raise StackUnavailable("the api container is not running")
        return containers[0]

    @classmethod
    def _prepare_workspace(cls) -> None:
        """Build the provider from the working tree and lay out a Terraform run.

        The binary is rebuilt every run. A provider binary that silently lags
        the source it is meant to be testing is worse than no test at all.
        """
        if cls.workspace.exists():
            shutil.rmtree(cls.workspace)
        plugins = cls.workspace / "plugins"
        config = cls.workspace / "config"
        plugins.mkdir(parents=True)
        config.mkdir(parents=True)

        build = docker(
            "run", "--rm",
            "-v", f"{ROOT}:/src:ro",
            "-v", f"{plugins}:/out",
            "-v", "ipam-gomod:/go/pkg/mod",
            "-w", "/src/providers/terraform",
            # The Terraform image is Alpine/musl, so a cgo-linked binary would
            # fail to exec there with a bare "no such file or directory".
            "-e", "CGO_ENABLED=0",
            GO_IMAGE, "go", "build", "-buildvcs=false",
            "-o", "/out/terraform-provider-platformipam", ".",
        )
        if build.returncode != 0:
            raise StackUnavailable(f"could not build the provider: {build.stderr[-500:]}")

        (cls.workspace / "terraform.rc").write_text(
            "provider_installation {\n"
            "  dev_overrides {\n"
            f'    "{PROVIDER_SOURCE}" = "/plugins"\n'
            "  }\n"
            "  direct {}\n"
            "}\n",
            encoding="utf-8",
        )
        (config / "main.tf").write_text(MAIN_TF % {"source": PROVIDER_SOURCE}, encoding="utf-8")

    def new_config(self, name: str):
        """A fresh Terraform state directory for one test.

        Allocation key is immutable and forces replacement, so tests sharing
        one state would destroy each other's allocations.
        """
        import shutil as _shutil

        directory = self.workspace / f"config-{name}"
        if directory.exists():
            _shutil.rmtree(directory)
        directory.mkdir(parents=True)
        (directory / "main.tf").write_text(MAIN_TF % {"source": PROVIDER_SOURCE},
                                           encoding="utf-8")
        return directory

    def terraform(self, *args: str, key: str = "", config=None,
                  expect_success: bool = True):
        """Run Terraform inside the API container's network namespace."""
        argv = [
            "run", "--rm",
            "--network", f"container:{self.api_container}",
            "-v", f"{config or (self.workspace / 'config')}:/work",
            "-v", f"{self.workspace / 'plugins'}:/plugins:ro",
            "-v", f"{self.workspace / 'terraform.rc'}:/terraform.rc:ro",
            "-w", "/work",
            "-e", "TF_CLI_CONFIG_FILE=/terraform.rc",
            "-e", "TF_IN_AUTOMATION=1",
            "-e", f"PLATFORM_IPAM_TOKEN={self.stack.token}",
            "-e", "PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1",
            "--entrypoint", "terraform",
            TERRAFORM_IMAGE, *args,
        ]
        if key:
            argv += ["-var", f"allocation_key={key}"]
        result = docker(*argv)
        if expect_success and result.returncode != 0:
            combined = result.stdout + result.stderr
            if "no matching manifest" in combined or "manifest unknown" in combined:
                raise StackUnavailable(f"{TERRAFORM_IMAGE} is unavailable")
            self.fail(
                f"terraform {' '.join(args)} failed:\n"
                f"{result.stdout[-1500:]}\n{result.stderr[-800:]}"
            )
        return result

    # -- tests ---------------------------------------------------------------

    def test_plan_reserves_nothing_and_apply_is_stable(self):
        key = run_key("terraform")
        config = self.new_config("stable")

        before = set(self.stack.netbox_prefixes())
        plan = self.terraform("plan", "-input=false", key=key, config=config)
        after_plan = set(self.stack.netbox_prefixes())
        self.assertEqual(before, after_plan, "planning must not reserve address space")
        # The CIDR cannot be known before apply; a provider that guessed one
        # would let a plan promise an address the ledger has not committed.
        self.assertIn("known after apply", plan.stdout)

        # dev_overrides is in force, so Terraform warns on every command. Keep
        # that visible (ADR 0004) rather than filtering it away.
        self.assertIn("development overrides", (plan.stdout + plan.stderr).lower())

        self.terraform("apply", "-auto-approve", "-input=false", key=key, config=config)
        outputs = json.loads(self.terraform("output", "-json", config=config).stdout)
        cidr = outputs["cidr"]["value"]
        allocation_id = outputs["allocation_id"]["value"]
        self.assertTrue(cidr, "apply must produce a committed CIDR")

        # The API and the operator inventory must agree with Terraform state.
        allocation = self.stack.api("GET", f"/v1/allocations/{allocation_id}").body
        self.assertEqual(allocation["cidr"], cidr)
        self.assertEqual(allocation["allocation_key"], key)
        self.assertIn(cidr, self.stack.netbox_prefixes())

        # A second plan must be empty: the allocation is already held.
        repeat = self.terraform("plan", "-input=false", "-detailed-exitcode",
                                key=key, config=config, expect_success=False)
        self.assertEqual(repeat.returncode, 0,
                         f"a second plan was not empty:\n{repeat.stdout[-1500:]}")

        import importlib.util

        spec = importlib.util.spec_from_file_location(
            "provider_plan_policy", ROOT / "scripts/ai/checks.py")
        policy = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(policy)

        # Import restores the held platform identity without claiming another
        # range. Read and refresh must preserve the same committed allocation.
        held_prefixes = set(self.stack.netbox_prefixes())
        self.terraform("state", "rm", "platformipam_allocation.vpc", config=config)
        self.terraform("import", "-input=false", "-var", f"allocation_key={key}",
                       "platformipam_allocation.vpc", allocation_id, config=config)
        imported = json.loads(self.terraform("show", "-json", config=config).stdout)
        resource = imported["values"]["root_module"]["resources"][0]["values"]
        self.assertEqual(resource["id"], allocation_id)
        self.assertEqual(resource["cidr"], cidr)
        self.assertEqual(set(self.stack.netbox_prefixes()), held_prefixes)
        refreshed = self.terraform("plan", "-refresh-only", "-input=false",
                                   "-detailed-exitcode", "-out=/work/refresh.tfplan",
                                   key=key, config=config,
                                   expect_success=False)
        self.assertIn(refreshed.returncode, (0, 2), refreshed.stdout[-1500:])
        refresh_plan = json.loads(self.terraform("show", "-json", "refresh.tfplan",
                                                  config=config).stdout)
        # Import can leave root outputs to reconcile even when the held
        # resource is unchanged. Inspect the saved refresh plan before applying
        # it, then verify no drift and a stable normal plan. Terraform 1.14's
        # refresh-only state comparison can falsely report exit2 (upstream
        # hashicorp/terraform#37886), so its JSON invariants carry that check.
        changes = refresh_plan.get("resource_changes", [])
        self.assertTrue(all(row["change"]["actions"] == ["no-op"] for row in changes),
                        f"refresh changed an imported resource: {changes}")
        self.assertFalse(refresh_plan.get("resource_drift"), refresh_plan)
        self.assertEqual(policy.replacement_errors(refresh_plan), [])
        self.terraform("apply", "-input=false", "refresh.tfplan", config=config)
        refreshed_state = json.loads(self.terraform("show", "-json", config=config).stdout)
        refreshed_resource = refreshed_state["values"]["root_module"]["resources"][0]["values"]
        self.assertEqual(refreshed_resource["id"], allocation_id)
        self.assertEqual(refreshed_resource["cidr"], cidr)
        stable_refresh = self.terraform("plan", "-refresh-only", "-input=false",
                                        "-detailed-exitcode", "-out=/work/stable-refresh.tfplan",
                                        key=key, config=config,
                                        expect_success=False)
        stable_refresh_json = json.loads(self.terraform("show", "-json", "stable-refresh.tfplan",
                                                        config=config).stdout)
        self.assertIn(stable_refresh.returncode, (0, 2), stable_refresh.stdout[-1500:])
        self.assertFalse(stable_refresh_json.get("errored"), stable_refresh_json)
        self.assertFalse(stable_refresh_json.get("resource_drift"), stable_refresh_json)
        self.assertTrue(all(row["change"]["actions"] == ["no-op"]
                            for row in stable_refresh_json.get("resource_changes", [])),
                        stable_refresh_json)
        self.assertTrue(all(row["actions"] == ["no-op"]
                            for row in stable_refresh_json.get("output_changes", {}).values()),
                        stable_refresh_json)
        self.assertEqual(policy.replacement_errors(stable_refresh_json), [])
        stable_normal = self.terraform("plan", "-input=false", "-detailed-exitcode",
                                       key=key, config=config, expect_success=False)
        self.assertEqual(stable_normal.returncode, 0, stable_normal.stdout[-1500:])
        self.assertEqual(set(self.stack.netbox_prefixes()), held_prefixes)

        # Terraform-core replacement causes need the required consumer plan
        # policy. Reject a forced same-key replacement, accept a distinct key,
        # and verify that neither plan changes inventory.
        unsafe = self.terraform("plan", "-input=false",
                                "-out=/work/same-key.tfplan",
                                "-replace=platformipam_allocation.vpc",
                                key=key, config=config)
        unsafe_plan = json.loads(self.terraform("show", "-json", "same-key.tfplan",
                                                config=config).stdout)
        self.assertEqual(policy.replacement_errors(unsafe_plan),
                         ["platformipam_allocation.vpc"])
        self.terraform("plan", "-input=false", "-out=/work/replacement.tfplan",
                       key=key + "-replacement", config=config)
        replacement = json.loads(self.terraform("show", "-json", "replacement.tfplan",
                                                config=config).stdout)
        change = next(row for row in replacement["resource_changes"]
                      if row["address"] == "platformipam_allocation.vpc")["change"]
        self.assertEqual(set(change["actions"]), {"create", "delete"})
        self.assertNotEqual(change["before"]["allocation_key"],
                            change["after"]["allocation_key"])
        self.assertEqual(policy.replacement_errors(replacement), [])
        self.assertEqual(set(self.stack.netbox_prefixes()), held_prefixes)

    def test_destroy_releases_the_allocation_without_freeing_it_immediately(self):
        key = run_key("terraform-destroy")
        config = self.new_config("destroy")
        self.terraform("apply", "-auto-approve", "-input=false", key=key, config=config)
        allocation_id = json.loads(
            self.terraform("output", "-json", config=config).stdout
        )["allocation_id"]["value"]

        self.terraform("destroy", "-auto-approve", "-input=false", key=key, config=config)

        # Destroy records release intent. It must not make the range instantly
        # reusable, and the allocation must not simply vanish.
        allocation = self.stack.api("GET", f"/v1/allocations/{allocation_id}").body
        self.assertIn(allocation["state"], {"QUARANTINED", "RELEASED"},
                      f"state after destroy was {allocation['state']}")

    @classmethod
    def tearDownClass(cls) -> None:
        # Leave the workspace when a run is being debugged.
        if os.environ.get("IPAM_E2E_KEEP_WORKSPACE") == "1":
            return
        shutil.rmtree(WORKSPACE, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
