"""Scoped local checks with compact JSON output and private temporary logs."""

import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]


class Report:
    def __init__(self, name):
        self.name = name
        self.started = time.monotonic()
        self.artifacts = Path(tempfile.mkdtemp(prefix=f"platform-ipam-{name}-"))
        self.checks = []

    def add(self, name, status, detail, log=None):
        item = {"check": name, "status": status, "detail": detail}
        if log:
            item["log"] = str(log)
        self.checks.append(item)

    def command(self, name, argv, cwd=ROOT, empty_output=False, env=None, timeout=120):
        if not shutil.which(str(argv[0])):
            self.add(name, "BLOCKED", f"Missing executable: {argv[0]}")
            return False
        log = self.artifacts / f"{len(self.checks):02d}-{name}.log"
        try:
            # Directory is 0700. Large output stays on disk, outside model context.
            with log.open("wb") as output:
                result = subprocess.run(argv, cwd=cwd, stdout=output, stderr=subprocess.STDOUT,
                                        timeout=timeout, env=env)
            passed = result.returncode == 0 and (not empty_output or log.stat().st_size == 0)
            self.add(name, "PASSED" if passed else "FAILED",
                     "Completed" if passed else f"Exit {result.returncode}; inspect log", log)
            return passed
        except (OSError, subprocess.TimeoutExpired) as error:
            self.add(name, "BLOCKED", type(error).__name__, log)
            return False

    def finish(self):
        states = {item["status"] for item in self.checks}
        status = "FAILED" if "FAILED" in states else "BLOCKED" if "BLOCKED" in states else "PASSED"
        print(json.dumps({"check": self.name, "status": status,
                          "duration_seconds": round(time.monotonic() - self.started, 3),
                          "checks": self.checks}))
        return {"PASSED": 0, "FAILED": 1, "BLOCKED": 2}[status]


def local_link_errors(paths):
    errors = []
    for path in paths:
        fenced = False
        for number, line in enumerate(path.read_text().splitlines(), 1):
            if line.lstrip().startswith(("```", "~~~")):
                fenced = not fenced
                continue
            if fenced:
                continue
            for target in re.findall(r"(?<!!)\[[^\]]+\]\(([^)]+)\)", line):
                target = target.strip().strip("<>")
                parsed = urlsplit(target)
                if parsed.scheme or parsed.netloc or not parsed.path:
                    continue
                if not (path.parent / unquote(parsed.path)).exists():
                    errors.append(f"{path.relative_to(ROOT)}:{number}: {target}")
    return errors


def contract(report, args):
    documents = [ROOT / "README.md", ROOT / "AGENTS.md"]
    documents += list((ROOT / "docs").rglob("*.md")) + list((ROOT / ".agents/skills").rglob("*.md"))
    errors = local_link_errors([path for path in documents if path.exists()])
    log = report.artifacts / "links.log"
    log.write_text("\n".join(errors))
    report.add("local-document-links", "FAILED" if errors else "PASSED",
               f"{len(errors)} broken local file links; URL reachability/anchors not checked", log if errors else None)

    examples = list((ROOT / "examples").rglob("*.json"))
    for path in examples:
        try:
            json.loads(path.read_text())
            report.add(str(path.relative_to(ROOT)), "PASSED", "JSON syntax only")
        except (OSError, ValueError) as error:
            report.add(str(path.relative_to(ROOT)), "FAILED", str(error))
    yaml_paths = list((ROOT / "examples/config").glob("*.yaml"))
    if yaml_paths:
        try:
            import yaml
        except ImportError:
            report.add("config-yaml", "BLOCKED", "PyYAML is not installed")
        else:
            for path in yaml_paths:
                try:
                    yaml.safe_load(path.read_text())
                    report.add(str(path.relative_to(ROOT)), "PASSED", "YAML syntax only")
                except (OSError, yaml.YAMLError) as error:
                    report.add(str(path.relative_to(ROOT)), "FAILED", str(error))

    schemas = [ROOT / f"api/openapi.{suffix}" for suffix in ("yaml", "yml", "json")]
    schemas = [path for path in schemas if path.exists()]
    if len(schemas) != 1:
        report.add("openapi", "BLOCKED", "Expected one api/openapi.yaml, .yml, or .json; contract implementation pending")
    else:
        relative = schemas[0].relative_to(ROOT).as_posix()
        if shutil.which("openapi-spec-validator"):
            report.command("openapi", ["openapi-spec-validator", str(schemas[0])])
        elif shutil.which("docker"):
            # Same contract as the host tool, without requiring it to be installed.
            report.command("openapi", containerized(
                [PYTHON_IMAGE, "sh", "-c",
                 "pip install --quiet openapi-spec-validator >/dev/null && "
                 f"openapi-spec-validator /src/{relative}"],
                workdir="/src"), cwd=ROOT)
        else:
            report.add("openapi", "BLOCKED",
                       "Neither openapi-spec-validator nor Docker is available")
    report.add("contract-behavior", "NOT_CHECKED", "Example/schema conformance, compatibility diff, and API behavior require implementation tests")


def aws(report, args):
    """docs/WORK_PLAN.md package M1b4: wires tests/aws/*.sh into this
    launcher alongside the other scripts/ai/check-* commands --
    tests/aws/test_org_inventory.sh (the collector alone, stubbed `aws` CLI,
    package B2/M1b1) and tests/aws/test_assess_from_inventory.sh (the true
    end-to-end run: the real collector into the real `platform-ipam onboard
    assess`, package M1b4). Both are pure bash and need no host Go toolchain
    -- test_assess_from_inventory.sh builds/runs the command through the
    pinned Go image when PLATFORM_IPAM_BIN is not set, exactly like this
    launcher's own `provider`/`helm` fallbacks.
    
    Package T1 adds shellcheck validation over scripts/aws/*.sh, tests/aws/*.sh,
    and deploy/compose/**/*.sh, with the same pinned-image fallback pattern."""
    directory = ROOT / "tests/aws"
    test_scripts = sorted(directory.glob("test_*.sh"))
    if not test_scripts:
        report.add("aws-tests", "BLOCKED", "No tests/aws/test_*.sh scripts exist")
        return
    if not shutil.which("bash"):
        report.add("aws-tests", "BLOCKED", "bash is not available")
        return
    for script in test_scripts:
        # PLATFORM_IPAM_BIN, when the caller has already built one (this
        # launcher does not build it itself), saves test_assess_from_inventory.sh
        # its one build of the binary through the pinned Go image.
        # test_assess_from_inventory.sh builds the binary through the pinned Go
        # image when PLATFORM_IPAM_BIN is not set; on a cold module cache or a
        # busy machine that alone takes minutes, and a timeout would read as
        # BLOCKED although nothing is wrong.
        report.command(script.stem, ["bash", str(script)], cwd=ROOT, env=dict(os.environ), timeout=540)
    
    # Shellcheck validation: host tool if available, pinned image otherwise
    all_shell_scripts = (
        sorted((ROOT / "scripts/aws").glob("*.sh")) +
        sorted((ROOT / "tests/aws").glob("*.sh")) +
        sorted((ROOT / "deploy/compose").rglob("*.sh"))
    )
    if not all_shell_scripts:
        report.add("shellcheck", "BLOCKED", "No shell scripts found to check")
        return
    
    # Always the pinned image where Docker exists, so a developer's machine and
    # CI judge the scripts with one shellcheck version; the host's own tool is
    # the fallback, not the preference.
    if shutil.which("docker"):
        for script in all_shell_scripts:
            report.command(f"shellcheck-{script.relative_to(ROOT).as_posix().replace('/', '-')}",
                          containerized([SHELLCHECK_IMAGE, f"/src/{script.relative_to(ROOT).as_posix()}"], workdir="/src"), cwd=ROOT)
    elif shutil.which("shellcheck"):
        for script in all_shell_scripts:
            report.command(f"shellcheck-{script.relative_to(ROOT).as_posix().replace('/', '-')}", ["shellcheck", str(script)], cwd=ROOT)
    else:
        report.add("shellcheck", "NOT_CHECKED", "Neither Docker nor shellcheck is available")
    
    report.add("live-aws-organization", "NOT_CHECKED",
               "Every scenario here runs against a stubbed aws CLI; nothing has run against a real AWS Organization")


def replacement_errors(plan):
    if not isinstance(plan, dict) or not isinstance(plan.get("format_version"), str):
        raise ValueError("Expected terraform show -json plan with format_version")
    if not re.fullmatch(r"1\.\d+", plan["format_version"]):
        raise ValueError("Only Terraform plan JSON format major version 1 is supported")
    if "planned_values" not in plan or "resource_changes" not in plan:
        raise ValueError("Plan must include planned_values and resource_changes; state JSON is not a plan")
    if not isinstance(plan["planned_values"], dict) or not isinstance(plan["resource_changes"], list):
        raise ValueError("Invalid planned_values or resource_changes")
    if plan.get("errored"):
        raise ValueError("Cannot approve an errored plan")
    if plan.get("complete") is False or plan.get("deferred_changes"):
        raise ValueError("Cannot approve an incomplete or deferred plan")
    errors = []
    for resource in plan["resource_changes"]:
        if not isinstance(resource, dict) or "type" not in resource or "mode" not in resource:
            raise ValueError("Malformed resource change")
        if resource["mode"] != "managed" or resource["type"] != "platformipam_allocation":
            continue
        change = resource["change"]
        actions = change["actions"]
        if not isinstance(actions, list) or not actions:
            raise ValueError("Missing allocation change actions")
        if "create" not in actions or "delete" not in actions:
            continue
        before, after = change.get("before") or {}, change.get("after") or {}
        unknown = change.get("after_unknown", {})
        key_unknown = unknown is True or (isinstance(unknown, dict) and unknown.get("allocation_key"))
        old, new = before.get("allocation_key"), after.get("allocation_key")
        if key_unknown or not isinstance(old, str) or not isinstance(new, str) or not new or old == new:
            errors.append(resource.get("address", "unknown allocation"))
    return errors


# Pinned images, matching ADR 0002: a check must not depend on the host
# happening to carry a correct Go or Terraform toolchain.
GO_IMAGE = "golang:1.26.8-bookworm"
TERRAFORM_IMAGE = "hashicorp/terraform:1.14"
HELM_IMAGE = "alpine/helm:3.17.3"
PYTHON_IMAGE = "python:3.13-alpine"
SHELLCHECK_IMAGE = "koalaman/shellcheck:v0.10.0"


def containerized(argv, workdir, mounts=(), env=()):
    """Wrap a command so it runs in a pinned image through Docker."""
    command = ["docker", "run", "--rm", "-v", f"{ROOT}:/src", "-w", workdir]
    for mount in mounts:
        command += ["-v", mount]
    for name, value in env:
        command += ["-e", f"{name}={value}"]
    return command + list(argv)


def terraform_ready(report):
    """Report whether a usable host Terraform exists.

    A missing or too-old host Terraform is not a failure: ADR 0002 runs
    Terraform from a pinned image, so the caller falls back to Docker.
    """
    terraform_ready.ok = False
    if not shutil.which("terraform"):
        report.add("terraform-version", "NOT_CHECKED",
                   "No host Terraform; the pinned image is used instead")
        return False
    try:
        result = subprocess.run(["terraform", "version"], capture_output=True, text=True, timeout=10)
        match = re.search(r"Terraform v(\d+)\.(\d+)\.(\d+)", result.stdout)
        version = tuple(map(int, match.groups())) if match else None
        if result.returncode or version is None or version < (1, 6, 0) or version[0] != 1:
            report.add("terraform-version", "NOT_CHECKED",
                       f"Host Terraform is {'.'.join(map(str, version)) if version else 'unrecognized'}; "
                       "the pinned image is used instead")
            return False
        report.add("terraform-version", "PASSED", ".".join(map(str, version)))
        terraform_ready.ok = True
        return True
    except (OSError, subprocess.TimeoutExpired):
        report.add("terraform-version", "NOT_CHECKED",
                   "Unable to determine the host Terraform version; the pinned image is used instead")
        return False


terraform_ready.ok = False


def provider(report, args):
    files = sorted((ROOT / "examples/terraform").rglob("*.tf"))
    use_docker = not terraform_ready(report) and shutil.which("docker")
    if files and (use_docker or terraform_ready.ok):
        for directory in sorted({path.parent for path in files}):
            relative = directory.relative_to(ROOT).as_posix()
            argv = (containerized(["--entrypoint", "terraform", TERRAFORM_IMAGE, "fmt", "-check"],
                                  workdir=f"/src/{relative}")
                    if use_docker else ["terraform", "fmt", "-check"])
            report.command(f"terraform-format-{directory.name}",
                           argv, cwd=ROOT if use_docker else directory)
    elif not files:
        report.add("terraform-format", "BLOCKED", "No Terraform examples exist")
    if args.plan_json:
        try:
            errors = replacement_errors(json.loads(args.plan_json.read_text()))
            report.add("replacement-keys", "FAILED" if errors else "PASSED",
                       "Unsafe allocation replacement: " + ", ".join(errors) if errors else "No same-key or unknown-key allocation replacements")
        except (OSError, ValueError, KeyError, TypeError) as error:
            report.add("replacement-keys", "FAILED", str(error))
    module = ROOT / "providers/terraform"
    if not (module / "go.mod").is_file():
        report.add("provider-unit-tests", "BLOCKED", "providers/terraform/go.mod does not exist yet")
    else:
        if shutil.which("go"):
            cache = os.environ.get("GOCACHE", str(Path(tempfile.gettempdir()) / f"platform-ipam-go-cache-{os.getuid()}"))
            env = dict(os.environ, TF_ACC="0", GOCACHE=cache)
            report.command("provider-unit-tests",
                           ["go", "test", "-short", "-mod=readonly", "./..."], cwd=module, env=env)
        elif shutil.which("docker"):
            report.command("provider-unit-tests", containerized(
                [GO_IMAGE, "go", "test", "-short", "-mod=readonly", "-buildvcs=false", "./..."],
                workdir="/src/providers/terraform",
                mounts=("ipam-gomod:/go/pkg/mod",),
                env=(("TF_ACC", "0"),),
            ), cwd=ROOT)
        else:
            report.add("provider-unit-tests", "BLOCKED",
                       "Neither a Go toolchain nor Docker is available")
    report.add("provider-acceptance", "NOT_CHECKED", "Real provider/API/AWS acceptance tests are separate; this command never enables TF_ACC")


def compose(report, args):
    directory = ROOT / "deploy/compose"
    base = directory / "compose.yaml"
    if not base.is_file():
        report.add("compose", "BLOCKED", "deploy/compose/compose.yaml does not exist yet")
        return
    combinations = [("compose-base", [base])]
    netbox = directory / "compose.netbox.yaml"
    dev = directory / "compose.dev.yaml"
    if netbox.exists():
        combinations.append(("compose-netbox", [base, netbox]))
        # NetBox refuses to start when SECRET_KEY or an API token pepper is
        # shorter than 50 characters, and the visible symptom is an unrelated
        # database wait timeout. Check the property rather than a fixed number
        # of occurrences, so adding a service cannot silently skip the guard
        # or fail the check for the wrong reason.
        text = netbox.read_text()
        for label, pattern in (
            ("secret-key", r"SECRET_KEY:\s+\$\{NETBOX_SECRET_KEY:-([^}]+)\}"),
            ("api-token-pepper", r"API_TOKEN_PEPPER_1:\s+\$\{NETBOX_API_TOKEN_PEPPER:-([^}]+)\}"),
        ):
            defaults = re.findall(pattern, text)
            short = [value for value in defaults if len(value) < 50]
            if not defaults:
                status, detail = "FAILED", f"No NetBox {label} default was found"
            elif short:
                status = "FAILED"
                detail = (f"{len(short)} of {len(defaults)} NetBox {label} defaults are "
                          "shorter than the 50-character minimum NetBox enforces")
            else:
                status = "PASSED"
                detail = f"All {len(defaults)} NetBox {label} defaults meet the 50-character minimum"
            report.add(f"netbox-{label}-default", status, detail)
        # Header trust in ui-proxy's `basic` mode (ADR 0006,
        # docs/GUI_AUTHENTICATION.md rule 1) is only safe if ui-proxy is the
        # sole published path to NetBox. Parsed with PyYAML rather than a
        # whole-file regex, so a service whose port mapping spans multiple
        # keys, or a comment mentioning "8080", cannot pass or fail this for
        # the wrong reason.
        try:
            import yaml
        except ImportError:
            report.add("netbox-port-not-published-outside-ui-proxy", "BLOCKED",
                       "PyYAML is not installed")
        else:
            try:
                netbox_doc = yaml.safe_load(text) or {}
            except yaml.YAMLError as error:
                report.add("netbox-port-not-published-outside-ui-proxy", "FAILED", str(error))
                netbox_doc = None
            if netbox_doc is not None:
                violators = []
                for service_name, service in (netbox_doc.get("services") or {}).items():
                    if service_name == "ui-proxy" or not isinstance(service, dict):
                        continue
                    for entry in service.get("ports") or []:
                        if isinstance(entry, dict):
                            target = str(entry.get("target", ""))
                            described = f"{entry.get('published', '?')}:{target}"
                        else:
                            described = str(entry)
                            target = described.rsplit(":", 1)[-1]
                        target_port = target.split("/", 1)[0]
                        if target_port == "8080" or "NETBOX_PORT" in described:
                            violators.append(f"{service_name} ({described})")
                status = "FAILED" if violators else "PASSED"
                detail = (f"Published alongside ui-proxy: {', '.join(violators)}" if violators
                         else "Only ui-proxy publishes a port mapped to NetBox's 8080/NETBOX_PORT")
                report.add("netbox-port-not-published-outside-ui-proxy", status, detail)
    if dev.exists():
        combinations.append(("compose-dev", [base, *([netbox] if netbox.exists() else []), dev]))
    plugin = directory / "compose.netbox-plugin.yaml"
    if plugin.exists() and netbox.exists():
        # Optional overlay (package N2). It only replaces the NetBox image, so
        # it is validated on top of the NetBox stack and never on its own.
        combinations.append(("compose-netbox-plugin", [base, netbox, plugin]))
    # Optional operator-UI auth modes (packages A4 and A5). Each is one overlay
    # on the NetBox stack; `basic` mode is the stack without either.
    for mode in ("entra", "ldap", "samba-ad"):
        overlay = directory / f"compose.ui-{mode}.yaml"
        if overlay.exists() and netbox.exists():
            combinations.append((f"compose-ui-{mode}", [base, netbox, overlay]))
    moto = directory / "compose.aws-moto.yaml"
    if moto.exists():
        combinations.append(("compose-aws-moto", [base, moto]))
    for version in ("4_6_7", "4_6_10"):
        netbox_compat = directory / f"compose.netbox-compat-{version}.yaml"
        if netbox_compat.exists() and netbox.exists():
            combinations.append((f"compose-netbox-compat-{version}", [base, netbox, netbox_compat]))
    for name, files in combinations:
        command = ["docker", "compose"]
        if args.env_file:
            command.extend(["--env-file", str(args.env_file.resolve())])
        for path in files:
            command.extend(["-f", str(path)])
        report.command(name, [*command, "config", "--quiet"])
    report.add("runtime", "NOT_CHECKED", "Container readiness, seed, and restart recovery have not been exercised")


def _helm_argv(kind, environment, values_paths, chart, chart_path):
    """Build a `helm lint`/`helm template` argv, host or containerized.

    values_paths is an ordered list of Path objects, each layered with its
    own -f (later files override earlier ones, same as the Helm CLI).
    """
    use_docker = not shutil.which("helm")
    if use_docker:
        flags = []
        for value in values_paths:
            flags += ["-f", f"/src/{value.relative_to(ROOT).as_posix()}"]
        if kind == "lint":
            return containerized([HELM_IMAGE, "lint", f"/src/{chart_path}", "--strict", *flags], workdir="/src")
        return containerized([HELM_IMAGE, "template", f"platform-ipam-{environment}",
                              f"/src/{chart_path}", *flags], workdir="/src")
    flags = []
    for value in values_paths:
        flags += ["-f", str(value)]
    if kind == "lint":
        return ["helm", "lint", str(chart), "--strict", *flags]
    return ["helm", "template", f"platform-ipam-{environment}", str(chart), *flags]


def _must_fail_render(report, name, argv, ok_detail, bad_detail, expect_in_log=None):
    """Run a `helm template` invocation that MUST be refused (a value
    combination the chart's schema or a template `fail` guard rejects).
    A non-zero exit is the correct outcome, so this PASSES when the render
    FAILS -- shared by the uiProxy TLS guard and work-plan package H3's
    operatorJob guards below.

    expect_in_log (work-plan package H7): when given, a failing render is
    PASSED only if this substring also appears in the combined output --
    asserting not just that the render was refused, but that it was refused
    for the stated reason, not some other one."""
    log = report.artifacts / f"{len(report.checks):02d}-{name}.log"
    try:
        with log.open("wb") as output:
            result = subprocess.run(argv, cwd=ROOT, stdout=output, stderr=subprocess.STDOUT, timeout=120)
        if result.returncode != 0:
            if expect_in_log is not None:
                text = log.read_text(errors="replace")
                if expect_in_log not in text:
                    report.add(name, "FAILED",
                               f"Rendering correctly failed, but the message did not contain {expect_in_log!r}; "
                               f"got: {text.strip()[-500:]!r}", log)
                    return
            report.add(name, "PASSED", ok_detail, log)
        else:
            report.add(name, "FAILED", bad_detail, log)
    except (OSError, subprocess.TimeoutExpired) as error:
        report.add(name, "BLOCKED", type(error).__name__, log)


def _run_captured(report, name, argv, cwd=ROOT, timeout=120):
    """Run argv and return (returncode, stdout_text). Logs combined output
    under report.artifacts like Report.command does, but adds no report
    entry itself -- callers combine the exit code with their own content
    assertions on stdout before deciding PASSED/FAILED."""
    log = report.artifacts / f"{len(report.checks):02d}-{name}.log"
    try:
        result = subprocess.run(argv, cwd=cwd, capture_output=True, text=True, timeout=timeout)
        log.write_text((result.stdout or "") + (result.stderr or ""))
        return result.returncode, result.stdout or "", log
    except (OSError, subprocess.TimeoutExpired) as error:
        log.write_text(f"{type(error).__name__}: {error}")
        return None, "", log


# Work-plan package H3: one entry per required deploy/helm/platform-ipam/ci/
# overlay, driving both the rendered-manifest assertions and (via --set
# overrides on top of these) the must-fail checks below. env_present/
# env_absent are environment variable NAMES only (never values) -- the
# least-privilege claim this package makes: onboard never gets the database
# URL or the auth/AWS/OIDC settings Settings.Validate would otherwise
# demand; adopt gets exactly what the worker Deployment gets.
_WORKER_ENV_NAMES = {
    "IPAM_DATABASE_URL", "IPAM_CONFIG_FILE", "IPAM_ENVIRONMENT", "IPAM_AUTH_MODE",
    "IPAM_NETBOX_URL", "IPAM_NETBOX_TOKEN", "IPAM_AWS_MODE", "IPAM_LISTEN_ADDR",
    "IPAM_OIDC_ISSUER", "IPAM_OIDC_AUDIENCE", "IPAM_IDENTITY_FILE",
}
_ONBOARD_ENV_NAMES = {"IPAM_CONFIG_FILE", "IPAM_ENVIRONMENT", "IPAM_NETBOX_URL", "IPAM_NETBOX_TOKEN"}
# Work-plan package N4: seed goes further than onboard -- it loads no pools
# configuration at all (internal/netbox.New is constructed with no
# Domains/Pools for this mode), so it lacks even IPAM_CONFIG_FILE.
_SEED_ENV_NAMES = {"IPAM_ENVIRONMENT", "IPAM_NETBOX_URL", "IPAM_NETBOX_TOKEN"}
# Work-plan packages H5 (internal/config.Settings.Validate becomes mode-aware)
# and H7 (the chart stops handing the Job settings adopt never reads): adopt
# authenticates no HTTP caller and never listens, in any of its three
# subcommands (plan, apply, abandon alike), so its Job's environment omits
# these four names -- asserted ABSENT for every adopt combination below,
# where the pre-H7 combinations asserted them present.
_ADOPT_DROPPED_ENV_NAMES = {"IPAM_AUTH_MODE", "IPAM_LISTEN_ADDR", "IPAM_OIDC_ISSUER", "IPAM_OIDC_AUDIENCE"}
_ADOPT_ENV_NAMES = _WORKER_ENV_NAMES - _ADOPT_DROPPED_ENV_NAMES
_OPERATOR_JOB_COMBOS = [
    {
        "name": "onboard-plan", "mode": "onboard", "command": "plan",
        "extra_args": ["--domain", "core"], "table_key": "table.json", "run_id": "ci-onboard-plan",
        "env_present": _ONBOARD_ENV_NAMES, "env_absent": _WORKER_ENV_NAMES - _ONBOARD_ENV_NAMES,
    },
    {
        "name": "onboard-apply", "mode": "onboard", "command": "apply",
        "extra_args": ["--domain", "core", "--batch", "ci-import"], "table_key": "table.json", "run_id": "ci-onboard-apply",
        "env_present": _ONBOARD_ENV_NAMES, "env_absent": _WORKER_ENV_NAMES - _ONBOARD_ENV_NAMES,
    },
    {
        "name": "adopt-plan", "mode": "adopt", "command": "plan",
        "extra_args": [], "table_key": "records.csv", "run_id": "ci-adopt-plan",
        "env_present": _ADOPT_ENV_NAMES, "env_absent": _ADOPT_DROPPED_ENV_NAMES,
        "identity_configured": True,
    },
    {
        "name": "adopt-apply", "mode": "adopt", "command": "apply",
        "extra_args": ["--operator", "ci-operator"], "table_key": "records.csv", "run_id": "ci-adopt-apply",
        "env_present": _ADOPT_ENV_NAMES, "env_absent": _ADOPT_DROPPED_ENV_NAMES,
        "identity_configured": True,
    },
    {
        # Work-plan package H7: adopt abandon (ADR 0012) takes NO table at
        # all -- table_key is None, which _check_operator_job_manifest below
        # reads as "assert no input ConfigMap volume/mount was rendered,
        # and no table path in args" rather than the opposite. This combo's
        # own ci overlay sets identity.existingConfigMap chart-wide (so
        # plan/apply's own combos above can share one stage values file with
        # it) -- work-plan package H9 asserts IPAM_IDENTITY_FILE is STILL
        # absent here even so, and identity_forbidden (below) asserts no
        # identity volume/mount either: abandon resolves no principal, so
        # mounting the var without the file (or the file without reading it)
        # is never correct for it.
        "name": "adopt-abandon", "mode": "adopt", "command": "abandon",
        "extra_args": ["--allocation-id", "ci-allocation", "--operator", "ci-operator", "--reason", "ci reason for abandon"],
        "table_key": None, "run_id": "ci-adopt-abandon",
        "env_present": _ADOPT_ENV_NAMES - {"IPAM_IDENTITY_FILE"},
        "env_absent": _ADOPT_DROPPED_ENV_NAMES | {"IPAM_IDENTITY_FILE"},
        "identity_forbidden": True, "identity_configured": True,
    },
    {
        "name": "adopt-abandon-dry-run", "mode": "adopt", "command": "abandon",
        "extra_args": ["--allocation-id", "ci-allocation", "--operator", "ci-operator", "--reason", "ci reason for abandon", "--dry-run"],
        "table_key": None, "run_id": "ci-adopt-abandon-dry-run",
        "env_present": _ADOPT_ENV_NAMES - {"IPAM_IDENTITY_FILE"},
        "env_absent": _ADOPT_DROPPED_ENV_NAMES | {"IPAM_IDENTITY_FILE"},
        "identity_forbidden": True, "identity_configured": True,
    },
    {
        # Work-plan package H9: `adopt abandon` resolves no domain.Principal
        # (internal/adoptcmd/abandon.go's runAbandon never receives cfg; ADR
        # 0012) and Settings.Validate("adopt") never requires
        # IPAM_IDENTITY_FILE either, so -- unlike "adopt-plan"/"adopt-apply"
        # above -- this combo renders successfully with NO
        # identity.existingConfigMap configured at all (see
        # ci/operator-job-adopt-abandon-no-identity-values.yaml) and must
        # mount no identity ConfigMap and carry no IPAM_IDENTITY_FILE.
        # identity_forbidden (read by _check_operator_job_manifest) asserts
        # the absence; no other combo sets it, so every other combo's
        # behaviour around the identity volume/mount is unchanged.
        "name": "adopt-abandon-no-identity", "mode": "adopt", "command": "abandon",
        "extra_args": ["--allocation-id", "ci-allocation", "--operator", "ci-operator", "--reason", "ci reason for abandon"],
        "table_key": None, "run_id": "ci-adopt-abandon-no-identity",
        "env_present": _ADOPT_ENV_NAMES - {"IPAM_IDENTITY_FILE"},
        "env_absent": _ADOPT_DROPPED_ENV_NAMES | {"IPAM_IDENTITY_FILE"},
        "identity_forbidden": True,
    },
    {
        # Work-plan package N4: seed (docs/WORK_PLAN.md, internal/seedcmd)
        # takes no subcommand and no flags -- command is "" and _never_
        # appended as a container arg (see _check_operator_job_manifest's
        # seed-specific expected_args below), and, like abandon, table_key is
        # None: it mounts no reviewed table. Unlike abandon it also resolves
        # no identity at all regardless of whether identity.existingConfigMap
        # is configured chart-wide (identity_forbidden), and unlike onboard
        # it needs no pools configuration either, so env_present is the
        # smallest of every combination in this list.
        "name": "seed", "mode": "seed", "command": "",
        "extra_args": [], "table_key": None, "run_id": "ci-seed",
        "env_present": _SEED_ENV_NAMES, "env_absent": _WORKER_ENV_NAMES - _SEED_ENV_NAMES,
        "identity_forbidden": True,
    },
]


def _operator_job_from_docs(docs):
    """The one Job doc that is the operator Job, distinct from the
    migration hook Job (whose name ends "-migrate", never "-operator-<runId>")."""
    for doc in docs:
        if isinstance(doc, dict) and doc.get("kind") == "Job" and "-operator-" in (doc.get("metadata") or {}).get("name", ""):
            return doc
    return None


def _deployment_by_component(docs, component):
    for doc in docs:
        if not isinstance(doc, dict) or doc.get("kind") != "Deployment":
            continue
        template_meta = ((doc.get("spec") or {}).get("template") or {}).get("metadata") or {}
        if (template_meta.get("labels") or {}).get("app.kubernetes.io/component") == component:
            return doc
    return None


def _service_account_by_name(docs, sa_name):
    for doc in docs:
        if isinstance(doc, dict) and doc.get("kind") == "ServiceAccount" and doc.get("metadata", {}).get("name") == sa_name:
            return doc
    return None


def _check_operator_job_manifest(report, name, combo, docs):
    """Every rendered-manifest assertion work-plan package H3 requires for
    one operatorJob combination, as one PASSED/FAILED entry naming every
    failing assertion in its detail."""
    problems = []
    job = _operator_job_from_docs(docs)
    if job is None:
        report.add(name, "FAILED", "No Job with '-operator-' in its name was rendered")
        return
    job_name = job.get("metadata", {}).get("name", "")
    if not job_name.endswith(f"-operator-{combo['run_id']}"):
        problems.append(f"Job name {job_name!r} does not end with -operator-{combo['run_id']}")
    annotations = job.get("metadata", {}).get("annotations") or {}
    if any(str(k).startswith("helm.sh/hook") for k in annotations):
        problems.append(f"Job carries a helm.sh/hook annotation {annotations!r}; it must not be a hook")
    spec = job.get("spec") or {}
    if spec.get("backoffLimit") != 0:
        problems.append(f"backoffLimit is {spec.get('backoffLimit')!r}, want 0")
    pod_spec = ((spec.get("template") or {}).get("spec")) or {}
    if pod_spec.get("restartPolicy") != "Never":
        problems.append(f"restartPolicy is {pod_spec.get('restartPolicy')!r}, want Never")
    containers = pod_spec.get("containers") or []
    if not containers:
        report.add(name, "FAILED", "; ".join(problems + ["Job pod template has no containers"]))
        return
    container = containers[0]
    # Work-plan package H7: a combo with no table_key (adopt abandon, ADR
    # 0012) takes NO positional table-path argument at all -- the third
    # argument is its own first flag, and no "input" volume/mount may exist.
    has_table = combo["table_key"] is not None
    if combo["mode"] == "seed":
        # seed (work-plan package N4) takes no subcommand at all -- unlike
        # every other mode, its command ("") is never appended as a second
        # argument (templates/operator-job.yaml's `{{- if not $isSeed }}`
        # guard around the command line).
        expected_args = [combo["mode"], *combo["extra_args"]]
    elif has_table:
        table_path = f"/var/run/platform-ipam/input/{combo['table_key']}"
        expected_args = [combo["mode"], combo["command"], table_path, *combo["extra_args"]]
    else:
        expected_args = [combo["mode"], combo["command"], *combo["extra_args"]]
    if container.get("args") != expected_args:
        problems.append(f"args are {container.get('args')!r}, want {expected_args!r}")
    env_names = {e.get("name") for e in (container.get("env") or [])}
    missing = combo["env_present"] - env_names
    if missing:
        problems.append(f"missing expected env var(s): {sorted(missing)}")
    unwanted = combo["env_absent"] & env_names
    if unwanted:
        problems.append(f"env var(s) present that must be absent for mode {combo['mode']}: {sorted(unwanted)}")
    api_deployment = _deployment_by_component(docs, "api")
    if api_deployment is None:
        problems.append("no api Deployment found in the same render to compare securityContext against")
    else:
        api_pod_spec = ((api_deployment.get("spec") or {}).get("template") or {}).get("spec") or {}
        api_container = (api_pod_spec.get("containers") or [{}])[0]
        if container.get("securityContext") != api_container.get("securityContext"):
            problems.append("container securityContext differs from the api Deployment's")
        if pod_spec.get("securityContext") != api_pod_spec.get("securityContext"):
            problems.append("pod securityContext differs from the api Deployment's")
        # Work-plan package H9: platform-ipam.workloadEnv (_helpers.tpl) grew
        # an "identity" parameter, defaulting to true so its two pre-existing
        # callers (the api/worker Deployments' own "full: true" includes,
        # which never pass this key) are unaffected. A combo whose ci overlay
        # configures identity.existingConfigMap (identity_configured) is the
        # one place that default is exercised for the Deployments in this
        # check suite -- nothing else in this file asserts they still carry
        # IPAM_IDENTITY_FILE, so a mutant flipping that default silently
        # (verified: it passes every other check here) would otherwise stop
        # the api/worker Deployments from mounting the operator role's
        # identity file whenever an operator identity is configured.
        if combo.get("identity_configured"):
            api_env_names = {e.get("name") for e in (api_container.get("env") or [])}
            if "IPAM_IDENTITY_FILE" not in api_env_names:
                problems.append("identity.existingConfigMap is configured but the api Deployment lacks IPAM_IDENTITY_FILE")
    volumes = pod_spec.get("volumes") or []
    input_volume = next((v for v in volumes if v.get("name") == "input"), None)
    volume_mounts = container.get("volumeMounts") or []
    input_mount = next((m for m in volume_mounts if m.get("name") == "input"), None)
    if has_table:
        if input_volume is None:
            problems.append("no 'input' ConfigMap volume was rendered")
        else:
            items = (input_volume.get("configMap") or {}).get("items") or []
            if not items or items[0].get("key") != combo["table_key"] or items[0].get("path") != combo["table_key"]:
                problems.append(f"input ConfigMap volume items are {items!r}, want key/path {combo['table_key']!r}")
        if input_mount is None:
            problems.append("no 'input' volumeMount was rendered")
    else:
        if input_volume is not None:
            problems.append(f"an 'input' ConfigMap volume was rendered for command {combo['command']!r}, which takes no table: {input_volume!r}")
        if input_mount is not None:
            problems.append(f"an 'input' volumeMount was rendered for command {combo['command']!r}, which takes no table: {input_mount!r}")
    # Work-plan package H9: a combo with identity_forbidden (adopt abandon
    # with no identity.existingConfigMap configured) must render no identity
    # ConfigMap volume or volumeMount at all -- abandon resolves no
    # domain.Principal and needs no identity file, so nothing should ever
    # mount one for it even where a future combo might set
    # identity.existingConfigMap chart-wide alongside it.
    if combo.get("identity_forbidden"):
        identity_volume = next((v for v in volumes if v.get("name") == "identity"), None)
        identity_mount = next((m for m in volume_mounts if m.get("name") == "identity"), None)
        if identity_volume is not None:
            problems.append(f"an 'identity' ConfigMap volume was rendered for command {combo['command']!r}, which resolves no principal and needs none: {identity_volume!r}")
        if identity_mount is not None:
            problems.append(f"an 'identity' volumeMount was rendered for command {combo['command']!r}, which resolves no principal and needs none: {identity_mount!r}")
    # The cloud role lives on a ServiceAccount annotation, not an env var:
    # onboard must never carry it, so it must run under a dedicated,
    # annotation-free ServiceAccount distinct from the worker's.
    sa_name = pod_spec.get("serviceAccountName")
    api_deployment_for_sa = _deployment_by_component(docs, "worker")
    worker_sa_name = (((api_deployment_for_sa.get("spec") or {}).get("template") or {}).get("spec") or {}).get(
        "serviceAccountName") if api_deployment_for_sa else None
    if combo["mode"] in ("onboard", "seed"):
        if sa_name == worker_sa_name:
            problems.append(f"{combo['mode']}'s Job runs as the worker's own ServiceAccount ({sa_name!r}); it must be a dedicated one")
        if pod_spec.get("automountServiceAccountToken") is not False:
            problems.append(f"{combo['mode']}'s Job does not set automountServiceAccountToken: false")
        sa_doc = _service_account_by_name(docs, sa_name) if sa_name else None
        if sa_doc is None:
            problems.append(f"no ServiceAccount named {sa_name!r} was rendered for {combo['mode']}'s dedicated identity")
        elif sa_doc.get("metadata", {}).get("annotations"):
            problems.append(f"{combo['mode']}'s dedicated ServiceAccount carries annotations (a possible cloud role): {sa_doc['metadata']['annotations']!r}")
    else:
        if sa_name != worker_sa_name:
            problems.append(f"adopt's Job runs as {sa_name!r}, want the worker's own ServiceAccount {worker_sa_name!r}")
        if pod_spec.get("automountServiceAccountToken") is not True:
            problems.append("adopt's Job does not set automountServiceAccountToken: true")
    # Least privilege on the network too (the ci overlays set a chart-wide
    # egress entry, 10.0.99.0/24, and onboard's own, 10.0.20.0/24): onboard's
    # NetworkPolicy must carry its own list and never the chart-wide one, which
    # is where the database and the cloud endpoints live; adopt's must be
    # exactly the worker's.
    def _egress_cidrs(component_suffix):
        for doc in docs:
            if (isinstance(doc, dict) and doc.get("kind") == "NetworkPolicy"
                    and (doc.get("metadata") or {}).get("name", "").endswith(component_suffix)):
                return sorted(
                    peer["ipBlock"]["cidr"]
                    for rule in (doc.get("spec") or {}).get("egress") or []
                    for peer in rule.get("to") or [] if "ipBlock" in peer)
        return None
    job_cidrs, worker_cidrs = _egress_cidrs("-operator-job"), _egress_cidrs("-worker")
    if job_cidrs is None:
        problems.append("no NetworkPolicy was rendered for the operator Job's pods")
    elif combo["mode"] in ("onboard", "seed"):
        if "10.0.99.0/24" in job_cidrs:
            problems.append(f"{combo['mode']}'s NetworkPolicy carries the chart-wide egress list: {job_cidrs!r}")
        if "10.0.20.0/24" not in job_cidrs:
            problems.append(f"{combo['mode']}'s NetworkPolicy lacks its own egress entry: {job_cidrs!r}")
    elif job_cidrs != worker_cidrs or "10.0.99.0/24" not in job_cidrs:
        problems.append(f"adopt's NetworkPolicy egress {job_cidrs!r} is not the worker's {worker_cidrs!r}")
    if problems:
        report.add(name, "FAILED", "; ".join(problems))
    else:
        report.add(name, "PASSED",
                   f"Job ...-operator-{combo['run_id']}: backoffLimit 0, restartPolicy Never, no hook "
                   f"annotation, args and env correct for mode {combo['mode']}, securityContext matches "
                   "the api Deployment's, input ConfigMap volume correct")


def _check_no_operator_job_when_disabled(report, name, text):
    """Work-plan package H3 requirement 8: with operatorJob.enabled left at
    its default (false), the rendered output must show no Job other than
    the migration hook Job, and no operator-job component label anywhere."""
    try:
        import yaml
    except ImportError:
        report.add(name, "BLOCKED", "PyYAML is not installed")
        return
    try:
        docs = [d for d in yaml.safe_load_all(text) if d]
    except yaml.YAMLError as error:
        report.add(name, "FAILED", f"Could not parse rendered YAML: {error}")
        return
    jobs = [d for d in docs if isinstance(d, dict) and d.get("kind") == "Job"]
    unexpected_jobs = [d.get("metadata", {}).get("name", "") for d in jobs
                       if not d.get("metadata", {}).get("name", "").endswith("-migrate")]
    has_label = "operator-job" in text
    if unexpected_jobs or has_label:
        detail = []
        if unexpected_jobs:
            detail.append(f"unexpected Job(s): {unexpected_jobs}")
        if has_label:
            detail.append("the string \"operator-job\" appears in the rendered output")
        report.add(name, "FAILED", "; ".join(detail))
    else:
        report.add(name, "PASSED", "Only the migration Job renders; no operator-job label appears")


def _check_deployments_keep_full_env(report, name, text):
    """The api and worker Deployments and the adopt Job share one env helper,
    and package H7 taught it to leave four variables out for the Job. The
    dangerous direction is the other one: a Deployment rendered without them
    -- the api refuses to start in stage and production without its OIDC
    settings. Byte-identity with the previous chart was checked by hand once;
    this is what keeps it true."""
    try:
        import yaml
    except ImportError:
        report.add(name, "BLOCKED", "PyYAML is not installed")
        return
    try:
        docs = [d for d in yaml.safe_load_all(text) if d]
    except yaml.YAMLError as error:
        report.add(name, "FAILED", f"Could not parse rendered YAML: {error}")
        return
    problems = []
    for component in ("api", "worker"):
        deployment = _deployment_by_component(docs, component)
        if deployment is None:
            problems.append(f"no {component} Deployment was rendered")
            continue
        containers = (((deployment.get("spec") or {}).get("template") or {}).get("spec") or {}).get("containers") or []
        names = {e.get("name") for c in containers for e in (c.get("env") or [])}
        missing = sorted(_ADOPT_DROPPED_ENV_NAMES - names)
        if missing:
            problems.append(f"the {component} Deployment lacks {missing}")
    if problems:
        report.add(name, "FAILED", "; ".join(problems))
    else:
        report.add(name, "PASSED", "api and worker Deployments carry the auth, listen and OIDC variables the adopt Job omits")


def helm(report, args):
    chart = ROOT / "deploy/helm/platform-ipam"
    if not (chart / "Chart.yaml").is_file():
        report.add("helm", "BLOCKED", "deploy/helm/platform-ipam/Chart.yaml does not exist yet")
        return
    chart_path = chart.relative_to(ROOT).as_posix()
    # Package A7: uiProxy is disabled in every committed environment values
    # file (it is optional, off by default), so the enabled path would
    # otherwise never be checked. This committed CI-only overlay flips it on
    # with valid values so it is additively lint/rendered alongside the real
    # environment values on every run.
    ui_proxy_values = chart / "ci/ui-proxy-values.yaml"
    docker_or_helm = shutil.which("helm") or shutil.which("docker")
    local_kind_values = chart / "ci/local-kind-values.yaml"
    if local_kind_values.is_file() and docker_or_helm:
        report.command("helm-lint-local-kind",
                       _helm_argv("lint", "stage", [local_kind_values], chart, chart_path), cwd=ROOT)
        report.command("helm-render-local-kind",
                       _helm_argv("template", "stage", [local_kind_values], chart, chart_path), cwd=ROOT)
    for environment in ("stage", "prod"):
        values = ROOT / f"deploy/environments/{environment}/values.yaml"
        if not values.is_file():
            report.add(f"helm-{environment}", "BLOCKED", f"Missing {values.relative_to(ROOT)}")
            continue
        if not docker_or_helm:
            report.add(f"helm-{environment}", "BLOCKED",
                       "Neither Helm nor Docker is available")
            continue
        report.command(f"helm-lint-{environment}",
                       _helm_argv("lint", environment, [values], chart, chart_path), cwd=ROOT)
        code, text, _log = _run_captured(
            report, f"helm-render-{environment}",
            _helm_argv("template", environment, [values], chart, chart_path))
        passed = code == 0
        report.add(f"helm-render-{environment}", "PASSED" if passed else "FAILED",
                   "Completed" if passed else f"Exit {code}; inspect log")
        # Work-plan package H3 requirement 8: disabled (the shipped default
        # for every committed environment values file) must render
        # byte-for-byte the same Job/label surface as before this package.
        if passed:
            _check_no_operator_job_when_disabled(report, f"helm-{environment}-no-operator-job-when-disabled", text)
            _check_deployments_keep_full_env(report, f"helm-{environment}-deployments-keep-full-env", text)
        if not ui_proxy_values.is_file():
            report.add(f"helm-{environment}-ui-proxy-enabled", "BLOCKED",
                       f"Missing {ui_proxy_values.relative_to(ROOT)}")
            continue
        report.command(f"helm-lint-{environment}-ui-proxy-enabled",
                       _helm_argv("lint", environment, [values, ui_proxy_values], chart, chart_path), cwd=ROOT)
        report.command(f"helm-render-{environment}-ui-proxy-enabled",
                       _helm_argv("template", environment, [values, ui_proxy_values], chart, chart_path), cwd=ROOT)

    # A rendering that MUST fail: HTTP Basic credentials must never cross the
    # wire in plaintext (ADR 0006 rule 7), so uiProxy.ingress.enabled with an
    # empty tlsSecretName has to be refused -- by values.schema.json's
    # conditional requirement, and, belt-and-braces in case schema
    # validation were ever bypassed, by `fail` in
    # templates/ui-proxy-ingress.yaml.
    stage_values = ROOT / "deploy/environments/stage/values.yaml"
    if not docker_or_helm:
        report.add("helm-ui-proxy-ingress-requires-tls", "BLOCKED", "Neither Helm nor Docker is available")
    elif not stage_values.is_file():
        report.add("helm-ui-proxy-ingress-requires-tls", "BLOCKED", f"Missing {stage_values.relative_to(ROOT)}")
    elif not ui_proxy_values.is_file():
        report.add("helm-ui-proxy-ingress-requires-tls", "BLOCKED", f"Missing {ui_proxy_values.relative_to(ROOT)}")
    else:
        argv = _helm_argv("template", "stage", [stage_values, ui_proxy_values], chart, chart_path)
        argv = argv + ["--set-string", "uiProxy.ingress.tlsSecretName="]
        _must_fail_render(report, "helm-ui-proxy-ingress-requires-tls", argv,
                          "Rendering correctly failed: uiProxy.ingress.enabled with an empty "
                          "tlsSecretName is refused",
                          "Rendering succeeded with uiProxy.ingress.enabled and an empty "
                          "tlsSecretName; this must be refused")

    # Work-plan packages H3/H7/N4: operatorJob (opt-in Job for `adopt`,
    # `onboard` or `seed` in the cluster). Every entry of
    # _OPERATOR_JOB_COMBOS is validated against stage only -- the
    # environment-specific interaction
    # (IPAM_ENVIRONMENT's value, oidc/live-AWS requirements) is already
    # covered by the base per-environment checks above; operatorJob's own
    # values are orthogonal to which environment they are layered on.
    if not docker_or_helm:
        report.add("helm-operator-job", "BLOCKED", "Neither Helm nor Docker is available")
    elif not stage_values.is_file():
        report.add("helm-operator-job", "BLOCKED", f"Missing {stage_values.relative_to(ROOT)}")
    else:
        try:
            import yaml
            have_yaml = True
        except ImportError:
            have_yaml = False
            report.add("helm-operator-job-yaml", "BLOCKED", "PyYAML is not installed; manifest assertions skipped")
        for combo in _OPERATOR_JOB_COMBOS:
            combo_values = chart / f"ci/operator-job-{combo['name']}-values.yaml"
            if not combo_values.is_file():
                report.add(f"helm-operator-job-{combo['name']}", "BLOCKED", f"Missing {combo_values.relative_to(ROOT)}")
                continue
            report.command(f"helm-lint-operator-job-{combo['name']}",
                           _helm_argv("lint", "stage", [stage_values, combo_values], chart, chart_path), cwd=ROOT)
            code, text, _log = _run_captured(
                report, f"helm-render-operator-job-{combo['name']}",
                _helm_argv("template", "stage", [stage_values, combo_values], chart, chart_path))
            if code != 0:
                report.add(f"helm-operator-job-{combo['name']}", "FAILED", f"Rendering failed with exit {code}; inspect log")
                continue
            if not have_yaml:
                continue
            try:
                docs = [d for d in yaml.safe_load_all(text) if d]
            except yaml.YAMLError as error:
                report.add(f"helm-operator-job-{combo['name']}", "FAILED", f"Could not parse rendered YAML: {error}")
                continue
            _check_operator_job_manifest(report, f"helm-operator-job-{combo['name']}", combo, docs)

        # The must-fail renders, each built from a valid combination above
        # with exactly one required value removed or corrupted.
        adopt_apply_values = chart / "ci/operator-job-adopt-apply-values.yaml"
        onboard_plan_values = chart / "ci/operator-job-onboard-plan-values.yaml"
        adopt_plan_values = chart / "ci/operator-job-adopt-plan-values.yaml"
        if adopt_apply_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, adopt_apply_values], chart, chart_path)
            argv += ["--set", "operatorJob.mode=delete"]
            _must_fail_render(report, "helm-operator-job-unknown-mode", argv,
                              "Rendering correctly failed: an unrecognized operatorJob.mode is refused",
                              "Rendering succeeded with an unrecognized operatorJob.mode; this must be refused")
        if onboard_plan_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, onboard_plan_values], chart, chart_path)
            argv += ["--set-string", "operatorJob.runId="]
            _must_fail_render(report, "helm-operator-job-requires-run-id", argv,
                              "Rendering correctly failed: operatorJob.enabled with an empty runId is refused",
                              "Rendering succeeded with operatorJob.enabled and an empty runId; this must be refused")

            argv = _helm_argv("template", "stage", [stage_values, onboard_plan_values], chart, chart_path)
            argv += ["--set-string", "operatorJob.input.existingConfigMap="]
            _must_fail_render(report, "helm-operator-job-requires-input-configmap", argv,
                              "Rendering correctly failed: operatorJob.enabled with no input.existingConfigMap is refused",
                              "Rendering succeeded with operatorJob.enabled and no input.existingConfigMap; this must be refused")
        if adopt_plan_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, adopt_plan_values], chart, chart_path)
            argv += ["--set-string", "identity.existingConfigMap="]
            _must_fail_render(report, "helm-operator-job-adopt-requires-identity", argv,
                              "Rendering correctly failed: operatorJob.mode=adopt with no identity.existingConfigMap is refused",
                              "Rendering succeeded with operatorJob.mode=adopt and no identity.existingConfigMap; this must be refused")

        # Work-plan package H7: the new abandon-specific guards, plus the
        # allow-list guard for an unknown command in an otherwise-valid mode
        # (distinct from helm-operator-job-unknown-mode above, which corrupts
        # operatorJob.mode itself). Each also asserts the FAILURE MESSAGE,
        # not just the exit code, via _must_fail_render's expect_in_log.
        adopt_abandon_values = chart / "ci/operator-job-adopt-abandon-values.yaml"
        if adopt_abandon_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, adopt_abandon_values], chart, chart_path)
            argv += ["--set-string", "operatorJob.input.existingConfigMap=platform-ipam-should-not-exist"]
            _must_fail_render(report, "helm-operator-job-abandon-rejects-input-configmap", argv,
                              "Rendering correctly failed: operatorJob.command=abandon with an input ConfigMap is refused",
                              "Rendering succeeded with operatorJob.command=abandon and an input ConfigMap; this must be refused",
                              expect_in_log='must not be set when operatorJob.command is "abandon"')

            argv = _helm_argv("template", "stage", [stage_values, adopt_abandon_values], chart, chart_path)
            argv += ["--set-json", 'operatorJob.args=["--operator","ci-operator","--reason","ci reason for abandon"]']
            _must_fail_render(report, "helm-operator-job-abandon-requires-allocation-id", argv,
                              "Rendering correctly failed: operatorJob.command=abandon without --allocation-id in args is refused",
                              "Rendering succeeded with operatorJob.command=abandon and no --allocation-id in args; this must be refused",
                              expect_in_log='must contain "--allocation-id"')

            argv = _helm_argv("template", "stage", [stage_values, adopt_abandon_values], chart, chart_path)
            argv += ["--set-json", 'operatorJob.args=["--allocation-id","ci-allocation","--reason","ci reason for abandon"]']
            _must_fail_render(report, "helm-operator-job-abandon-requires-operator", argv,
                              "Rendering correctly failed: operatorJob.command=abandon without --operator in args is refused",
                              "Rendering succeeded with operatorJob.command=abandon and no --operator in args; this must be refused",
                              expect_in_log='must contain "--operator"')

            argv = _helm_argv("template", "stage", [stage_values, adopt_abandon_values], chart, chart_path)
            argv += ["--set-json", 'operatorJob.args=["--allocation-id","ci-allocation","--operator","ci-operator"]']
            _must_fail_render(report, "helm-operator-job-abandon-requires-reason", argv,
                              "Rendering correctly failed: operatorJob.command=abandon without --reason in args is refused",
                              "Rendering succeeded with operatorJob.command=abandon and no --reason in args; this must be refused",
                              expect_in_log='must contain "--reason"')

            argv = _helm_argv("template", "stage", [stage_values, adopt_abandon_values], chart, chart_path)
            argv += ["--set", "operatorJob.command=frobnicate"]
            _must_fail_render(report, "helm-operator-job-unknown-command-adopt", argv,
                              "Rendering correctly failed: an unrecognized operatorJob.command for mode=adopt is refused",
                              "Rendering succeeded with an unrecognized operatorJob.command for mode=adopt; this must be refused",
                              expect_in_log='is not supported for operatorJob.mode "adopt"')
        if onboard_plan_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, onboard_plan_values], chart, chart_path)
            argv += ["--set", "operatorJob.command=drift"]
            _must_fail_render(report, "helm-operator-job-unknown-command-onboard", argv,
                              "Rendering correctly failed: an unrecognized operatorJob.command for mode=onboard (e.g. \"drift\") is refused",
                              "Rendering succeeded with an unrecognized operatorJob.command for mode=onboard; this must be refused",
                              expect_in_log='is not supported for operatorJob.mode "onboard"')

        # Work-plan package N4: seed's own guards -- the OPPOSITE shape of
        # adopt/onboard's "command is required" guard, since seed takes no
        # subcommand, no flags and no table at all.
        seed_values = chart / "ci/operator-job-seed-values.yaml"
        if seed_values.is_file():
            argv = _helm_argv("template", "stage", [stage_values, seed_values], chart, chart_path)
            argv += ["--set", "operatorJob.command=plan"]
            _must_fail_render(report, "helm-operator-job-seed-rejects-command", argv,
                              "Rendering correctly failed: operatorJob.mode=seed with a command set is refused",
                              "Rendering succeeded with operatorJob.mode=seed and a command set; this must be refused",
                              expect_in_log='must not be set when operatorJob.mode is "seed"')

            argv = _helm_argv("template", "stage", [stage_values, seed_values], chart, chart_path)
            argv += ["--set-json", 'operatorJob.args=["--table","table.json"]']
            _must_fail_render(report, "helm-operator-job-seed-rejects-args", argv,
                              "Rendering correctly failed: operatorJob.mode=seed with non-empty args is refused",
                              "Rendering succeeded with operatorJob.mode=seed and non-empty args; this must be refused",
                              expect_in_log='must be empty when operatorJob.mode is "seed"')

            argv = _helm_argv("template", "stage", [stage_values, seed_values], chart, chart_path)
            argv += ["--set-string", "operatorJob.input.existingConfigMap=platform-ipam-should-not-exist",
                     "--set-string", "operatorJob.input.key=table.json"]
            _must_fail_render(report, "helm-operator-job-seed-rejects-input-configmap", argv,
                              "Rendering correctly failed: operatorJob.mode=seed with an input ConfigMap is refused",
                              "Rendering succeeded with operatorJob.mode=seed and an input ConfigMap; this must be refused",
                              expect_in_log='neither reads a reviewed table')

    report.add("cluster-verification", "NOT_CHECKED", "Cluster schema, identity, secrets, migrations, and rollout readiness require separate verification")


def main(name):
    parser = argparse.ArgumentParser(description=__doc__)
    if name == "provider":
        parser.add_argument("--plan-json", type=Path, help="Saved terraform show -json plan; read-only replacement-key guard")
    if name == "compose":
        parser.add_argument("--env-file", type=Path, help="Explicit local development interpolation file; values are not printed")
    args = parser.parse_args()
    report = Report(name)
    try:
        {"contract": contract, "provider": provider, "compose": compose, "helm": helm, "aws": aws}[name](report, args)
    except (OSError, ValueError, TypeError) as error:
        report.add("input", "FAILED", str(error))
    return report.finish()
