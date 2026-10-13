#!/usr/bin/env sh
# Exercise normal Terraform provider installation from a local filesystem mirror.
# The source address and version are test fixtures, not a publication decision.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
VERSION=${IPAM_PROVIDER_VERSION:-0.1.0}
SOURCE=${IPAM_PROVIDER_SOURCE:-registry.example.com/platform/platformipam}
TERRAFORM_IMAGE=${IPAM_TERRAFORM_IMAGE:-hashicorp/terraform:1.14.9}
EVIDENCE=${IPAM_PROVIDER_MIRROR_EVIDENCE:-}
python3 - "$ROOT" "$VERSION" "$SOURCE" "$TERRAFORM_IMAGE" "$EVIDENCE" <<'PY'
from pathlib import Path
import re
import sys
sys.path.insert(0, str(Path(sys.argv[1]) / 'scripts/release'))
from provider_release import names
names(sys.argv[2])
parts = sys.argv[3].split('/')
if (len(parts) != 3 or any(not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]*[a-z0-9])?', part)
                         for part in parts[1:])
        or any(not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]*[a-z0-9])?', label)
               for label in parts[0].split('.'))):
    raise SystemExit('provider source must have safe hostname/namespace/type components')
if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._:/@-]*', sys.argv[4]):
    raise SystemExit('Terraform image must be a safe container image reference')
if sys.argv[5]:
    evidence = Path(sys.argv[5])
    if any(path.is_symlink() for path in (evidence, *evidence.parents)):
        raise SystemExit('evidence directory must not contain symlinks')
    if evidence.exists() and (not evidence.is_dir() or any(evidence.iterdir())):
        raise SystemExit('evidence directory must be absent or empty')
    evidence.mkdir(parents=True, exist_ok=True)
PY
WORK=$(mktemp -d /tmp/platform-ipam-provider-mirror.XXXXXX)
cleanup() {
  mirror_status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$EVIDENCE" ]; then
    for evidence_file in init.log reinit.log schema.json config/.terraform.lock.hcl tampered.log terraform-version.json package-metadata.json installed-notices.json; do
      if [ -f "$WORK/$evidence_file" ]; then
        cp "$WORK/$evidence_file" "$EVIDENCE/${evidence_file##*/}" || mirror_status=1
      fi
    done
  fi
  docker run --rm -v "$WORK/config:/work" --entrypoint rm \
    "$TERRAFORM_IMAGE" -rf /work/.terraform >/dev/null 2>&1 || true
  rm -rf "$WORK"
  exit "$mirror_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$WORK/build" "$WORK/mirror/$SOURCE" "$WORK/config"

PACKAGE="$WORK/mirror/$SOURCE/terraform-provider-platformipam_${VERSION}_linux_amd64.zip"
BINARY="terraform-provider-platformipam_v${VERSION}"
if [ -n "${IPAM_PROVIDER_PACKAGE:-}" ]; then
  cp "$IPAM_PROVIDER_PACKAGE" "$PACKAGE"
  python3 - "$PACKAGE" "$WORK/build/$BINARY" "$VERSION" "$ROOT" <<'PY'
from pathlib import Path
import sys
sys.path.insert(0, str(Path(sys.argv[4]) / 'scripts/release'))
from provider_release import check_archive
Path(sys.argv[2]).write_bytes(check_archive(sys.argv[1], sys.argv[3]))
PY
else
  docker run --rm \
    -v "$ROOT:/src:ro" -v "$WORK/build:/out" -v ipam-gomod:/go/pkg/mod \
    -w /src/providers/terraform -e CGO_ENABLED=0 \
    golang:1.26.9-bookworm go build -mod=readonly -buildvcs=false -trimpath \
    -ldflags "-X main.version=$VERSION -X main.providerAddress=$SOURCE" -o "/out/$BINARY" .
  zip -q -j "$PACKAGE" "$WORK/build/$BINARY"
fi
cat > "$WORK/terraform.rc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "/mirror"
    include = ["$SOURCE"]
  }
  direct {
    exclude = ["$SOURCE"]
  }
}
EOF
cat > "$WORK/config/main.tf" <<EOF
terraform {
  required_providers {
    platformipam = {
      source  = "$SOURCE"
      version = "$VERSION"
    }
  }
}
EOF

terraform() {
  docker run --rm -v "$WORK/config:/work" -v "$WORK/mirror:/mirror:ro" \
    -v "$WORK/terraform.rc:/terraform.rc:ro" -w /work \
    -e TF_CLI_CONFIG_FILE=/terraform.rc -e TF_IN_AUTOMATION=1 \
    --entrypoint terraform "$TERRAFORM_IMAGE" "$@"
}

terraform version -json > "$WORK/terraform-version.json"
python3 - "$WORK/terraform-version.json" "$PACKAGE" "$SOURCE" "$VERSION" "$WORK/package-metadata.json" <<'PY'
import hashlib
import json
from pathlib import Path
import re
import sys
cli_version = json.loads(Path(sys.argv[1]).read_text())['terraform_version']
if not isinstance(cli_version, str) or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', cli_version):
    raise SystemExit('Terraform must report its exact stable version')
with Path(sys.argv[2]).open('rb') as stream:
    package_sha256 = hashlib.file_digest(stream, 'sha256').hexdigest()
metadata = {'provider_source': sys.argv[3], 'provider_version': sys.argv[4],
            'terraform_version': cli_version, 'platform': 'linux_amd64',
            'package_sha256': package_sha256, 'published_origin_acceptance': False}
body = json.dumps(metadata, sort_keys=True)
Path(sys.argv[5]).write_text(body + '\n')
print(body)
PY
if ! terraform init -input=false -no-color > "$WORK/init.log" 2>&1; then
  tail -n 40 "$WORK/init.log" >&2
  exit 1
fi
python3 - "$ROOT" "$PACKAGE" "$VERSION" "$SOURCE" "$WORK/config/.terraform/providers" "$WORK/installed-notices.json" <<'PYNOTICES'
import hashlib
import json
from pathlib import Path
import sys
from zipfile import ZipFile
sys.path.insert(0, str(Path(sys.argv[1]) / 'scripts/release'))
from provider_release import check_archive, names
package, version, source = Path(sys.argv[2]), sys.argv[3], sys.argv[4]
check_archive(package, version)
installed = Path(sys.argv[5]) / source / version / 'linux_amd64'
with ZipFile(package) as archive:
    notice_names = sorted(set(archive.namelist()) - {names(version)['binary']})
    if notice_names:
        hashes = {}
        for name in notice_names:
            path = installed / name
            if path.is_symlink() or not path.is_file() or path.read_bytes() != archive.read(name):
                raise SystemExit('installed notice missing or changed: ' + name)
            hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()
        record = {'provider_source': source, 'provider_version': version,
                  'platform': 'linux_amd64', 'installed_notice_count': len(hashes),
                  'files_sha256': hashes, 'status': 'PASSED'}
        Path(sys.argv[6]).write_text(json.dumps(record, indent=2, sort_keys=True) + '\n')
        print(f'installed provider notices: {len(hashes)} files retained byte-for-byte')
PYNOTICES
python3 - "$WORK/config/.terraform.lock.hcl" "$VERSION" "$SOURCE" <<'PY'
from pathlib import Path
import sys
lock = Path(sys.argv[1]).read_text()
assert f'provider "{sys.argv[3]}"' in lock
assert f'version     = "{sys.argv[2]}"' in lock
assert 'h1:' in lock or 'zh:' in lock
PY
terraform providers schema -json > "$WORK/schema.json"
python3 - "$WORK/schema.json" "$SOURCE" <<'PY'
import json
import sys
schema = json.load(open(sys.argv[1], encoding='utf-8'))
assert sys.argv[2] in schema['provider_schemas']
PY
if ! terraform init -input=false -lockfile=readonly -no-color > "$WORK/reinit.log" 2>&1; then
  tail -n 40 "$WORK/reinit.log" >&2
  exit 1
fi

# Terraform must reject a changed package under the previously generated lock.
docker run --rm -v "$WORK/config:/work" --entrypoint rm \
  "$TERRAFORM_IMAGE" -rf /work/.terraform
docker run --rm -v "$WORK/build:/build" --entrypoint sh \
  "$TERRAFORM_IMAGE" -c \
  'printf "\nchanged provider binary\n" >> "/build/$1"' sh "$BINARY"
rm "$PACKAGE"
zip -q -j "$PACKAGE" "$WORK/build/$BINARY"
if terraform init -input=false -lockfile=readonly -no-color > "$WORK/tampered.log" 2>&1; then
  echo "tampered provider package was accepted" >&2
  exit 1
fi
# Standard hosted runners provide grep; ripgrep is not a prerequisite here.
if ! grep -Eiq 'checksum|does not match|doesn.t match' "$WORK/tampered.log"; then
  cat "$WORK/tampered.log" >&2
  echo "tampered provider package failed for an unexpected reason" >&2
  exit 1
fi
echo "provider mirror: install, schema, readonly lock, and checksum rejection passed"
