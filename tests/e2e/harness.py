"""Shared machinery for the end-to-end suite described in ADR 0002.

The suite drives a running Compose stack through the surfaces a consumer
actually uses -- REST, the first-party CLI, the Terraform provider, and the
NetBox inventory an operator reads -- and asserts that they agree about the
same allocation.

Two transports are supported for reaching the stack. On an ordinary developer
host the published loopback port is reachable and `urllib` is used directly.
Where loopback publishing is not reachable (a sandboxed or remote Docker
daemon), the same requests are issued from inside the stack's own network. The
tests do not care which is in use.

Since package A3, `NETBOX_PORT` publishes `ui-proxy`, not NetBox itself
(ADR 0006, docs/GUI_AUTHENTICATION.md): the proxy refuses `/api/` and
`/graphql/`, so `netbox()`/`netbox_as()` below always reach NetBox on the
internal network (`http://netbox:8080`), regardless of which transport is in
use for the platform API. `ui()` is the counterpart for tests that want to
exercise the proxy itself -- basic auth, header stripping, the API block --
through the published path.
"""

from __future__ import annotations

import base64
import json
import os
import shlex
import subprocess
import time
import unittest
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
COMPOSE_DIR = ROOT / "deploy/compose"
ENV_FILE = COMPOSE_DIR / ".env"
COMPOSE_FILES = ("compose.yaml", "compose.netbox.yaml")

# A single command timeout for every stack interaction. Long enough for a
# durable operation to settle, short enough that a wedged stack fails the run
# instead of hanging it.
COMMAND_TIMEOUT = 180


class StackUnavailable(unittest.SkipTest):
    """Raised when the development stack is not running.

    The suite skips rather than fails, so that a unit-test run on a machine
    with no stack does not report a false defect. `run-e2e.sh` starts the
    stack first, so a skip there means the harness itself is misconfigured.
    """


def load_env() -> dict:
    if not ENV_FILE.exists():
        raise StackUnavailable(f"{ENV_FILE} is missing; run deploy/compose/create-env.sh")
    values = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        name, _, value = line.partition("=")
        values[name.strip()] = value.strip()
    return values


def compose_argv(*args: str) -> list:
    argv = ["docker", "compose", "--env-file", str(ENV_FILE)]
    for name in COMPOSE_FILES:
        argv += ["-f", str(COMPOSE_DIR / name)]
    return argv + list(args)


def run(argv: list, timeout: int = COMMAND_TIMEOUT,
        stdin: str = None) -> subprocess.CompletedProcess:
    """Run argv against the Compose project. `stdin`, when given, is piped to
    the process's own stdin (package C7 -- `platform-ipam onboard parse -`
    reads its input that way); every pre-existing caller omits it, and
    `subprocess.run(..., input=None)` behaves exactly like not passing
    `input` at all, so this is purely additive.
    """
    return subprocess.run(
        argv, cwd=COMPOSE_DIR, capture_output=True, text=True, timeout=timeout,
        input=stdin,
    )


@dataclass(frozen=True)
class Response:
    status: int
    body: object
    headers: dict  # keys lower-cased, e.g. headers.get("www-authenticate")


class Stack:
    """A running development stack, addressed through whichever transport works."""

    def __init__(self) -> None:
        self.env = load_env()
        self.token = self.env.get("IPAM_LOCAL_TOKEN", "")
        self.netbox_token = self.env.get("IPAM_NETBOX_TOKEN", "")
        if not self.token or not self.netbox_token:
            raise StackUnavailable("IPAM_LOCAL_TOKEN and IPAM_NETBOX_TOKEN must be set in .env")
        self.api_port = self.env.get("IPAM_LISTEN_ADDR", ":8080").rsplit(":", 1)[-1]
        self.netbox_port = self.env.get("NETBOX_PORT", "18000")
        self.netbox_internal = self.env.get("IPAM_NETBOX_URL", "http://netbox:8080")
        self.ui_user = self.env.get("NETBOX_UI_USER", "")
        self.ui_password = self.env.get("NETBOX_UI_PASSWORD", "")
        self._direct = self._probe_direct()
        self._ui_direct = self._probe_ui_direct()
        self._require_ready()

    @property
    def ops_token(self) -> str:
        """The second local development identity's token (package G3c).

        Read lazily rather than in __init__: a test that never needs the
        ops-observer identity must not skip just because an older .env
        predates IPAM_OPS_TOKEN.
        """
        token = self.env.get("IPAM_OPS_TOKEN", "")
        if not token:
            raise StackUnavailable(
                "IPAM_OPS_TOKEN must be set in .env; "
                "regenerate it with deploy/compose/create-env.sh"
            )
        return token

    @property
    def operator_token(self) -> str:
        """The operator identity's token (package G3b1).

        Read lazily, like `ops_token`: a test that never needs the
        ops-operator identity must not skip just because an older .env
        predates IPAM_OPERATOR_TOKEN.
        """
        token = self.env.get("IPAM_OPERATOR_TOKEN", "")
        if not token:
            raise StackUnavailable(
                "IPAM_OPERATOR_TOKEN must be set in .env; "
                "regenerate it with deploy/compose/create-env.sh"
            )
        return token

    # -- transport selection -------------------------------------------------

    def _probe_direct(self) -> bool:
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{self.api_port}/readyz", timeout=5
            ) as response:
                return response.status == 200
        except (urllib.error.URLError, OSError, ValueError):
            return False

    def _probe_ui_direct(self) -> bool:
        """Whether ui-proxy's published port is reachable from this host.

        ui-proxy's unauthenticated /healthz (deploy/compose/ui-proxy/Caddyfile)
        exists exactly so this probe, like `_probe_direct` above, never needs
        a credential.
        """
        try:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{self.netbox_port}/healthz", timeout=5
            ) as response:
                return response.status == 200
        except (urllib.error.URLError, OSError, ValueError):
            return False

    def _require_ready(self) -> None:
        if self._direct:
            return
        result = run(compose_argv("exec", "-T", "api", "curl", "-fsS", "-m", "10",
                                  f"http://127.0.0.1:{self.api_port}/readyz"))
        if result.returncode != 0:
            raise StackUnavailable(
                "the platform API is not reachable directly or through Compose; "
                f"start the stack first ({result.stderr.strip()[:200]})"
            )

    # -- HTTP ----------------------------------------------------------------

    def api(self, method: str, path: str, body: object = None,
            headers: dict = None, *, token: str = None) -> Response:
        """Issue an authenticated request against the platform API.

        `token` (package G3c) lets a caller authenticate as a different
        configured identity, e.g. `self.stack.ops_token`; it defaults to
        today's behaviour (`self.token`, the primary development identity).
        """
        merged = {"Authorization": f"Bearer {token if token is not None else self.token}",
                 "Accept": "application/json"}
        merged.update(headers or {})
        base = f"http://127.0.0.1:{self.api_port}"
        return self._http(base, base, path, method, body, merged)

    def netbox(self, path: str) -> Response:
        """Read the NetBox inventory an operator sees in the UI.

        Asserting through NetBox's REST API rather than a browser is the
        default UI coverage chosen in ADR 0002: it is the same data the UI
        renders, and it is deterministic.

        Since package A3, `NETBOX_PORT` publishes ui-proxy, which refuses
        `/api/`, so this always goes over the internal network to NetBox
        itself -- never through `self._direct`/the published port, unlike the
        platform API calls below. `ui()` is the way to reach NetBox *through*
        the proxy.
        """
        headers = {"Authorization": f"Token {self.netbox_token}", "Accept": "application/json"}
        return self._http_in_container(self.netbox_internal + path, "GET", None, headers)

    def netbox_as(self, token: str, method: str, path: str, body: object = None) -> Response:
        """Issue a NetBox API request authenticated with an explicit token.

        Unlike `netbox()`, which always reads as the platform adapter's own
        service account, this lets a test exercise a different NetBox user --
        for example the `platform-operators` viewer bootstrapped for
        `test_e2e_netbox_roles.py` -- against any method, not only GET. Like
        `netbox()`, this always uses the internal network (see that
        docstring).
        """
        headers = {"Authorization": f"Token {token}", "Accept": "application/json"}
        return self._http_in_container(self.netbox_internal + path, method, body, headers)

    def ui(self, path: str = "/", user: str = None, password: str = None,
           headers: dict = None, method: str = "GET", path_as_is: bool = False) -> Response:
        """Issue a request through ui-proxy, the only published path to the
        NetBox UI (package A3, docs/GUI_AUTHENTICATION.md).

        Unlike `netbox()`/`netbox_as()`, which always talk to NetBox directly
        and never cross the proxy, this exercises the proxy's own trust
        boundary: basic auth against the bcrypt hash, stripping of any
        inbound `X-Remote-User`/`X-Remote-User-Group`, and the `/api/` and
        `/graphql/` block. Pass `user`/`password` for a valid credential;
        leave both `None` for an unauthenticated request (a wrong password is
        just a mismatched `password`). `headers` lets a caller add its own,
        e.g. a forged `X-Remote-User`, for the header-trust tests package A6
        adds. Returns the raw `Response`, including a 401's
        `WWW-Authenticate` header, so callers can assert on it directly.

        `path_as_is` (package A6): forwards curl's `--path-as-is` flag when
        the in-container transport is in use. curl's URL parser otherwise
        collapses `/./` and `/../` segments before the request is even sent,
        which would silently turn a path-based bypass attempt (e.g.
        `/static/../api/...`) into the ordinary, already-blocked path and
        prove nothing about the proxy. The direct transport (`urllib`, used
        when the published port is reachable from the host) never performs
        that normalization on its own, so this flag has nothing to do there.
        """
        merged = dict(headers or {})
        if user is not None or password is not None:
            credential = base64.b64encode(f"{user or ''}:{password or ''}".encode()).decode()
            merged["Authorization"] = f"Basic {credential}"
        if self._ui_direct:
            return self._http_direct(f"http://127.0.0.1:{self.netbox_port}{path}", method, None, merged)
        return self._http_in_container(f"http://ui-proxy:8080{path}", method, None, merged,
                                       path_as_is=path_as_is)

    def _http(self, direct_base: str, internal_base: str, path: str, method: str,
              body: object, headers: dict) -> Response:
        if self._direct:
            return self._http_direct(direct_base + path, method, body, headers)
        return self._http_in_container(internal_base + path, method, body, headers)

    def _http_direct(self, url: str, method: str, body: object,
                     headers: dict) -> Response:
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers = {**headers, "Content-Type": "application/json"}
        request = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return Response(response.status, _decode(response.read()),
                                {name.lower(): value for name, value in response.headers.items()})
        except urllib.error.HTTPError as error:
            return Response(error.code, _decode(error.read()),
                            {name.lower(): value for name, value in error.headers.items()})

    def _http_in_container(self, url: str, method: str, body: object,
                           headers: dict, path_as_is: bool = False) -> Response:
        # A unique separator keeps the status line unambiguous even when the
        # response body itself contains newlines. `-D -` dumps the response
        # headers to the same stdout, ahead of the body, so a 401's
        # WWW-Authenticate is visible to callers such as `ui()` even when this
        # transport (rather than `_http_direct`) is the one in use.
        marker = "---e2e-status---"
        argv = ["curl", "-sS", "-m", "30", "-D", "-", "-X", method,
                "-w", f"\\n{marker}%{{http_code}}"]
        if path_as_is:
            # See `ui()`'s docstring: without this, curl silently squashes
            # `/./` and `/../` path segments before the request is sent.
            argv.append("--path-as-is")
        argv.append(url)
        for name, value in headers.items():
            argv += ["-H", f"{name}: {value}"]
        if body is not None:
            argv += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
        result = run(compose_argv("exec", "-T", "api", *argv))
        if result.returncode != 0:
            raise AssertionError(f"request failed: {result.stderr.strip()[:400]}")
        combined, _, status = result.stdout.rpartition(marker)
        header_text, separator, payload = combined.partition("\r\n\r\n")
        if not separator:
            header_text, separator, payload = combined.partition("\n\n")
        response_headers = {}
        for line in header_text.splitlines()[1:]:  # [0] is the "HTTP/1.1 <code> ..." status line
            name, sep, value = line.partition(":")
            if sep:
                response_headers[name.strip().lower()] = value.strip()
        return Response(int(status.strip() or 0), _decode(payload.encode()), response_headers)

    # -- CLI -----------------------------------------------------------------

    def cli(self, *args: str, expect_exit: int = 0, token: str = None) -> object:
        """Run the first-party CLI (ADR 0003) inside the stack.

        `token` (package G3c) mirrors `api()`'s parameter: defaults to
        today's behaviour (`self.token`).
        """
        argv = compose_argv(
            "exec", "-T",
            "-e", f"PLATFORM_IPAM_URL=http://127.0.0.1:{self.api_port}",
            "-e", f"PLATFORM_IPAM_TOKEN={token if token is not None else self.token}",
            "-e", "PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1",
            "api", "platform-ipam", "client", *args,
        )
        result = run(argv)
        if result.returncode != expect_exit:
            raise AssertionError(
                f"`client {' '.join(shlex.quote(a) for a in args)}` exited "
                f"{result.returncode}, expected {expect_exit}\n"
                f"stdout: {result.stdout[:600]}\nstderr: {result.stderr[:600]}"
            )
        if not result.stdout.strip():
            return None
        return json.loads(result.stdout)

    def onboard(self, *args: str, stdin: str = None, expect_exit: int = 0):
        """Run `platform-ipam onboard <args>` inside the `api` container
        (package C7, docs/ONBOARDING_IMPORT.md section 2).

        Unlike `cli()` above, this sets no PLATFORM_IPAM_* environment: the
        `api` service's own Compose environment already carries
        IPAM_CONFIG_FILE, IPAM_NETBOX_URL and IPAM_NETBOX_TOKEN
        (deploy/compose/compose.yaml), which onboard reads directly -- it is
        a process mode, not a client of the platform API
        (docs/ONBOARDING_IMPORT.md section 2: "there is no API endpoint for
        it to call"). `stdin`, when given, is piped to the command's own
        stdin: onboard's `parse` subcommand reads a text table (CSV/TSV/
        paste) from stdin when its input is "-" (design section 2/3).
        Returns (stdout, stderr) on a match with `expect_exit` (0 by
        default); raises AssertionError otherwise, in the same style as
        `cli()`.
        """
        argv = compose_argv("exec", "-T", "api", "platform-ipam", "onboard", *args)
        result = run(argv, stdin=stdin)
        if result.returncode != expect_exit:
            raise AssertionError(
                f"`onboard {' '.join(shlex.quote(a) for a in args)}` exited "
                f"{result.returncode}, expected {expect_exit}\n"
                f"stdout: {result.stdout[:2000]}\nstderr: {result.stderr[:2000]}"
            )
        return result.stdout, result.stderr

    def adopt(self, *args: str, files: dict = None, expect_exit: int = 0):
        """Run `platform-ipam adopt <args>` inside the `api` container
        (package F5, docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md,
        docs/WORK_PLAN.md package F4).

        Modelled on `onboard()` above: this is a one-off run of the api
        image's own binary sharing the `api` service's own Compose
        environment (IPAM_DATABASE_URL, IPAM_CONFIG_FILE, IPAM_NETBOX_URL,
        IPAM_NETBOX_TOKEN, IPAM_FAKE_CLOUD_FILE, ...) -- `adopt` opens the
        ledger and the cloud observer itself, so it dispatches where `api`/
        `worker` do (cmd/platform-ipam/main.go), never through the platform
        API or through internal/service directly.

        `files`, when given, is a `{container_path: content}` mapping written
        into the `api` container before the command runs: unlike `onboard`'s
        `parse -` (which reads its table from stdin), `adopt plan|apply`
        takes the reviewed table as a real file path argument
        (internal/adoptcmd/adoptcmd.go opens it with os.Open), so the table
        has to exist inside the container first. Each file is written with a
        separate `sh -c 'cat > path'`, piping `content` to its stdin exactly
        as `stdin=` already does for `onboard`/`cli` above.
        """
        for path, content in (files or {}).items():
            write = run(compose_argv("exec", "-T", "api", "sh", "-c", f"cat > {shlex.quote(path)}"),
                       stdin=content)
            if write.returncode != 0:
                raise AssertionError(
                    f"writing {path} into the api container failed: {write.stderr[:400]}"
                )
        argv = compose_argv("exec", "-T", "api", "platform-ipam", "adopt", *args)
        result = run(argv)
        if result.returncode != expect_exit:
            raise AssertionError(
                f"`adopt {' '.join(shlex.quote(a) for a in args)}` exited "
                f"{result.returncode}, expected {expect_exit}\n"
                f"stdout: {result.stdout[:2000]}\nstderr: {result.stderr[:2000]}"
            )
        return result.stdout, result.stderr

    # -- convenience ---------------------------------------------------------

    def reserve(self, key: str, prefix_length: int, scope: str = "vpc",
                **extra: object) -> dict:
        """Reserve through REST with the idempotency discipline the contract requires."""
        body = {
            "allocation_key": key,
            "scope": scope,
            "environment": self.env.get("IPAM_ENVIRONMENT", "development"),
            "region": "eu-central-1",
            "account_id": "000000000000",
            "prefix_length": prefix_length,
            "description": "",
            "labels": {},
        }
        body.update(extra)
        response = self.api("POST", "/v1/allocations", body,
                            {"Idempotency-Key": f"e2e-{key}"})
        if response.status == 202:
            return self.await_operation(response.body)
        if response.status not in (200, 201):
            raise AssertionError(f"reserve {key} returned {response.status}: {response.body}")
        return response.body

    def await_operation(self, accepted: object, deadline: float = 120.0) -> dict:
        """Poll a durable operation to a terminal state, never re-posting."""
        operation_id = (accepted or {}).get("id")
        if not operation_id:
            raise AssertionError(f"202 response carried no operation id: {accepted}")
        end = time.monotonic() + deadline
        while time.monotonic() < end:
            response = self.api("GET", f"/v1/operations/{operation_id}")
            status = str((response.body or {}).get("status", "")).upper()
            if status in {"SUCCEEDED", "FAILED", "CANCELLED", "CANCELED"}:
                allocation_id = (response.body or {}).get("allocation_id")
                if status != "SUCCEEDED" or not allocation_id:
                    raise AssertionError(f"operation {operation_id} ended {status}: {response.body}")
                return self.api("GET", f"/v1/allocations/{allocation_id}").body
            time.sleep(1)
        raise AssertionError(f"operation {operation_id} did not settle within {deadline}s")

    def netbox_prefixes(self) -> dict:
        """Return NetBox prefixes keyed by CIDR."""
        response = self.netbox("/api/ipam/prefixes/?limit=200")
        if response.status != 200:
            raise AssertionError(f"NetBox prefixes returned {response.status}: {response.body}")
        return {row["prefix"]: row for row in (response.body or {}).get("results", [])}

    def capacity(self, pool_id: str) -> dict:
        response = self.api("GET", f"/v1/pools/{pool_id}/capacity")
        if response.status != 200:
            raise AssertionError(f"capacity returned {response.status}: {response.body}")
        return response.body

    def capacity_when_complete(self, pool_id: str, deadline: float = 60.0) -> dict:
        """A capacity reading taken while the evidence behind it is complete.

        `capacity` reports every size as 0 whenever the snapshot or the
        observation it rests on is incomplete -- correct, and momentary while a
        worker pass is between scans. A test that compares two readings must
        take both this way, or it compares the estate with a zero: that is how
        test_e2e_import's test_10 failed once (package H2a's run, 2026-09-20)
        and passed on the re-run.
        """
        end = time.monotonic() + deadline
        while True:
            reading = self.capacity(pool_id)
            if reading.get("complete") is True:
                return reading
            if time.monotonic() >= end:
                raise AssertionError(
                    f"capacity of {pool_id} never became complete within {deadline:.0f}s: {reading}")
            time.sleep(2)


def _decode(payload: bytes) -> object:
    text = payload.decode(errors="replace").strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except ValueError:
        return text


_STACK = None


def stack() -> Stack:
    """Return the shared stack handle, building it once per process."""
    global _STACK
    if _STACK is None:
        _STACK = Stack()
    return _STACK


def run_key(name: str) -> str:
    """Build a run-scoped allocation key.

    Allocation keys are permanent, so a suite that reused one fixed key would
    only ever exercise the replay path after its first run. IPAM_E2E_RUN_ID
    makes each run a distinct logical identity; `run-e2e.sh` sets it, and a
    manual run gets a time-based default.
    """
    run_id = os.environ.get("IPAM_E2E_RUN_ID") or f"local{int(time.time())}"
    return f"e2e-{run_id}-{name}"
