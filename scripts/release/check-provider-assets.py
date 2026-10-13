#!/usr/bin/env python3
"""Check Registry archive, executable, manifest and checksums before signing."""

import argparse
from pathlib import Path
import sys
import zipfile

from provider_release import check_assets, check_signature


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--signing-fingerprint", help="also verify the binary signature with this registered key")
    args = parser.parse_args()
    try:
        check_assets(args.directory, args.version)
        if args.signing_fingerprint:
            check_signature(args.directory, args.version, args.signing_fingerprint)
    except (OSError, UnicodeError, ValueError, zipfile.BadZipFile) as error:
        print(f"provider assets: {error}", file=sys.stderr)
        return 1
    print("provider assets: package, linux_amd64 executable, protocol 6.0 and checksums passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
