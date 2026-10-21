"""AWS IPAM authority calls shared by the pilot handoff and its verifier.

Every call goes through the AWS CLI with the caller's ambient credentials, in
the pool's locale region, which AWS requires for allocations. Only
``allocate`` writes; nothing here releases an allocation.
"""

import hashlib
import ipaddress
import json
import re
import subprocess

POOL_ID = re.compile(r"^ipam-pool-[0-9a-f]{8,32}$")
IDENTITY_PREFIX = "platform-ipam:"
TRANSIENT = ("Throttling", "RequestLimitExceeded", "InternalError", "ServiceUnavailable",
             "Unavailable", "RequestTimeout", "timed out", "Could not connect", "Connection was closed")


class AwsCallError(ValueError):
    def __init__(self, message, transient):
        super().__init__(message)
        self.transient = transient


def identity(tenant_id, allocation_key):
    """The allocation description that names its reviewed request."""
    if not tenant_id or not allocation_key or ":" in tenant_id:
        raise ValueError("an AWS IPAM request needs a tenant and allocation key")
    return f"{IDENTITY_PREFIX}{tenant_id}:{allocation_key}"


def parse_identity(description):
    if not isinstance(description, str) or not description.startswith(IDENTITY_PREFIX):
        return None
    tenant, _, key = description[len(IDENTITY_PREFIX):].partition(":")
    return (tenant, key) if tenant and key else None


def client_token(aws_pool_id, tenant_id, allocation_key, prefix_length):
    """A stable idempotency token: the same reviewed request retries as one."""
    material = "\n".join((aws_pool_id, tenant_id, allocation_key, str(prefix_length)))
    return "pipam-" + hashlib.sha256(material.encode()).hexdigest()[:58]


def run(arguments, runner=subprocess.run):
    completed = runner(["aws", *arguments, "--output", "json"], capture_output=True, text=True, timeout=60)
    if completed.returncode != 0:
        detail = (completed.stderr or "").strip().splitlines()
        message = detail[-1] if detail else f"aws exited {completed.returncode}"
        raise AwsCallError(message, any(marker in message for marker in TRANSIENT))
    try:
        return json.loads(completed.stdout or "{}")
    except json.JSONDecodeError as error:
        raise AwsCallError("aws returned unreadable JSON", False) from error


def pool_allocations(aws_pool_id, region, runner=subprocess.run):
    """All allocations in one pool. The read is eventually consistent."""
    rows = []
    token = None
    seen = set()
    while True:
        arguments = ["ec2", "get-ipam-pool-allocations", "--region", region,
                     "--ipam-pool-id", aws_pool_id, "--max-results", "1000", "--no-paginate"]
        if token:
            arguments += ["--next-token", token]
        page = run(arguments, runner)
        items = page.get("IpamPoolAllocations")
        if not isinstance(items, list):
            raise AwsCallError("AWS IPAM returned an unreadable allocation page", False)
        rows.extend(items)
        token = page.get("NextToken")
        if not token:
            return rows
        if token in seen:
            raise AwsCallError("AWS IPAM repeated a pagination token", False)
        seen.add(token)


def allocate(aws_pool_id, region, prefix_length, token, description, disallowed, runner=subprocess.run):
    arguments = ["ec2", "allocate-ipam-pool-cidr", "--region", region, "--ipam-pool-id", aws_pool_id,
                 "--netmask-length", str(prefix_length), "--client-token", token,
                 "--description", description]
    if disallowed:
        arguments += ["--disallowed-cidrs", *disallowed]
    response = run(arguments, runner).get("IpamPoolAllocation")
    if not isinstance(response, dict):
        raise AwsCallError("AWS IPAM returned no allocation", False)
    return response


def owned_rows(allocations, aws_pool_id):
    """Allocations carrying a Platform-IPAM request identity, keyed by it."""
    indexed = {}
    for item in allocations:
        parsed = parse_identity(item.get("Description"))
        if parsed:
            indexed.setdefault(parsed, []).append({
                "id": item.get("IpamPoolAllocationId"), "cidr": item.get("Cidr"),
                "resource_type": item.get("ResourceType"), "aws_ipam_pool_id": aws_pool_id,
                "description": item.get("Description")})
    return indexed


def relevant(pool_cidr, cidrs):
    """Distinct CIDRs that overlap the pool, as AWS DisallowedCidrs values."""
    pool = ipaddress.ip_network(pool_cidr, strict=True)
    found = set()
    for value in cidrs:
        network = ipaddress.ip_network(value, strict=True)
        if network.version == 4 and network.overlaps(pool):
            found.add(network)
    return [str(item) for item in sorted(found)]
