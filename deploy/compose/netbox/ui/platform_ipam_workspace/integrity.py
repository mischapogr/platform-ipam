"""Checks for operator-saved derived report snapshots."""

import hashlib
import json


def snapshot_digest_matches(snapshot):
    """Check the stored report before displaying or exporting a snapshot."""
    body = json.dumps(snapshot.report, sort_keys=True, indent=2) + "\n"
    return hashlib.sha256(body.encode("utf-8")).hexdigest() == snapshot.report_sha256
