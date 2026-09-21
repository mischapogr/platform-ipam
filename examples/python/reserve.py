#!/usr/bin/env python3
"""Illustrative client for the proposed platform-ipam API; no live API is shipped."""

import hashlib
import ipaddress
import json
import os
import random
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Keep bearer credentials on the configured origin.
        return None


class Client:
    def __init__(self, origin, token, deadline_seconds=600):
        parsed = urllib.parse.urlsplit(origin)
        local_http = (
            os.environ.get("PLATFORM_IPAM_ALLOW_LOCAL_HTTP") == "1"
            and parsed.scheme == "http"
            and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
        )
        if (
            (parsed.scheme != "https" and not local_http)
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.path not in {"", "/"}
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError("Use an HTTPS origin, or explicitly enabled loopback HTTP.")
        if not token or any(char.isspace() for char in token):
            raise ValueError("A non-empty bearer token without whitespace is required.")
        self.origin = origin.rstrip("/")
        self.token = token
        self.deadline = time.monotonic() + deadline_seconds
        self.opener = urllib.request.build_opener(NoRedirect())

    def remaining(self):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Deadline exceeded; recover with the same allocation key.")
        return remaining

    def pause(self, suggested, attempt=0):
        try:
            delay = float(suggested)
        except (TypeError, ValueError):
            delay = min(30, 2 ** min(attempt, 5)) + random.uniform(0, 1)
        # Honor a longer Retry-After by exhausting the overall deadline if needed.
        delay = max(0.1, delay)
        time.sleep(min(delay, self.remaining()))
        self.remaining()

    def request(self, method, path, payload=None, key=None):
        data = None if payload is None else json.dumps(
            payload, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")
        headers = {
            "Authorization": f"Bearer {self.token}",
            "Accept": "application/json",
        }
        if data is not None:
            headers["Content-Type"] = "application/json"
        if key is not None:
            headers["Idempotency-Key"] = key
        attempt = 0
        while True:
            request = urllib.request.Request(
                self.origin + path, data=data, headers=headers, method=method
            )
            try:
                with self.opener.open(request, timeout=min(30, self.remaining())) as response:
                    body = json.load(response)
                    if not isinstance(body, dict):
                        raise RuntimeError("Expected an API JSON object.")
                    return response.status, body, response.headers.get("Retry-After")
            except urllib.error.HTTPError as error:
                with error:
                    status = error.code
                    retry_after = error.headers.get("Retry-After")
                    request_id = error.headers.get("X-Request-ID", "unavailable")
                if status not in {429, 502, 503, 504}:
                    raise RuntimeError(
                        f"API returned HTTP {status}; request ID {request_id}. "
                        "Do not change the allocation key to bypass an error."
                    ) from None
                self.pause(retry_after, attempt)
            except (urllib.error.URLError, TimeoutError):
                # Reuse the exact same request body and idempotency key.
                self.pause(None, attempt)
            attempt += 1

    def reserve(self, payload):
        canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        request_key = "reserve-" + hashlib.sha256(canonical.encode("utf-8")).hexdigest()
        status, result, retry_after = self.request(
            "POST", "/v1/allocations", payload, request_key
        )
        if status == 202:
            operation_id = result["id"]
            path = "/v1/operations/" + urllib.parse.quote(operation_id, safe="")
            while result["status"] == "PENDING":
                self.pause(retry_after or "2")
                _, result, retry_after = self.request("GET", path)
            if result["status"] != "SUCCEEDED":
                raise RuntimeError(
                    f"Operation {operation_id} failed; inspect it before retrying."
                )
            allocation_id = result["result"]["allocation_id"]
            _, result, _ = self.request(
                "GET", "/v1/allocations/" + urllib.parse.quote(allocation_id, safe="")
            )
        elif status not in {200, 201}:
            raise RuntimeError(f"Unexpected reservation response: {status}.")

        if result["state"] not in {"RESERVED", "ACTIVE"}:
            raise RuntimeError("The returned allocation is not usable for provisioning.")
        network = ipaddress.ip_network(result["cidr"], strict=True)
        if network.version != 4 or network.prefixlen != payload["prefix_length"]:
            raise RuntimeError("Returned CIDR does not match the IPv4 request.")
        for field in (
            "allocation_key", "scope", "environment", "region", "account_id",
            "address_family", "parent_allocation_id", "availability_zone_id",
        ):
            if field in payload and result.get(field) != payload[field]:
                raise RuntimeError(f"Returned allocation has a different {field}.")
        return result


def main():
    if len(sys.argv) != 2:
        raise ValueError("Usage: reserve.py path/to/allocation-request.json")
    payload = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    client = Client(os.environ["PLATFORM_IPAM_URL"], os.environ["PLATFORM_IPAM_TOKEN"])
    print(json.dumps(client.reserve(payload), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (KeyError, TypeError, ValueError, RuntimeError, OSError) as error:
        print(f"Reservation incomplete: {error}", file=sys.stderr)
        sys.exit(1)
