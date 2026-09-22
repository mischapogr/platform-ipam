#!/usr/bin/env sh
# Exercise normal Terraform provider installation from a local filesystem mirror.
# The source address and version are test fixtures, not a publication decision.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
WORK=$(mktemp -d /tmp/platform-ipam-provider-mirror.XXXXXX)
cleanup() {
  docker run --rm -v "$WORK/config:/work" --entrypoint rm \
    hashicorp/terraform:1.14 -rf /work/.terraform >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT HUP INT TERM
mkdir -p "$WORK/build" "$WORK/mirror/registry.example.com/platform/platformipam" "$WORK/config"

docker run --rm \
  -v "$ROOT:/src:ro" -v "$WORK/build:/out" -v ipam-gomod:/go/pkg/mod \
  -w /src/providers/terraform -e CGO_ENABLED=0 \
  golang:1.26.8-bookworm go build -buildvcs=false \
  -o /out/terraform-provider-platformipam_v0.1.0 .

PACKAGE="$WORK/mirror/registry.example.com/platform/platformipam/terraform-provider-platformipam_0.1.0_linux_amd64.zip"
zip -q -j "$PACKAGE" "$WORK/build/terraform-provider-platformipam_v0.1.0"
cat > "$WORK/terraform.rc" <<'EOF'
provider_installation {
  filesystem_mirror {
    path    = "/mirror"
    include = ["registry.example.com/platform/platformipam"]
  }
  direct {
    exclude = ["registry.example.com/platform/platformipam"]
  }
}
EOF
cat > "$WORK/config/main.tf" <<'EOF'
terraform {
  required_providers {
    platformipam = {
      source  = "registry.example.com/platform/platformipam"
      version = "0.1.0"
    }
  }
}
EOF

terraform() {
  docker run --rm -v "$WORK/config:/work" -v "$WORK/mirror:/mirror:ro" \
    -v "$WORK/terraform.rc:/terraform.rc:ro" -w /work \
    -e TF_CLI_CONFIG_FILE=/terraform.rc -e TF_IN_AUTOMATION=1 \
    --entrypoint terraform hashicorp/terraform:1.14 "$@"
}

terraform init -input=false -no-color > "$WORK/init.log"
python3 - "$WORK/config/.terraform.lock.hcl" <<'PY'
from pathlib import Path
import sys
lock = Path(sys.argv[1]).read_text()
assert 'provider "registry.example.com/platform/platformipam"' in lock
assert 'version     = "0.1.0"' in lock
assert 'h1:' in lock or 'zh:' in lock
PY
terraform providers schema -json > "$WORK/schema.json"
python3 - "$WORK/schema.json" <<'PY'
import json
import sys
schema = json.load(open(sys.argv[1], encoding='utf-8'))
assert 'registry.example.com/platform/platformipam' in schema['provider_schemas']
PY
terraform init -input=false -lockfile=readonly -no-color > "$WORK/reinit.log"

# Terraform must reject a changed package under the previously generated lock.
docker run --rm -v "$WORK/config:/work" --entrypoint rm \
  hashicorp/terraform:1.14 -rf /work/.terraform
docker run --rm -v "$WORK/build:/build" --entrypoint sh \
  hashicorp/terraform:1.14 -c \
  'printf "\nchanged provider binary\n" >> /build/terraform-provider-platformipam_v0.1.0'
rm "$PACKAGE"
zip -q -j "$PACKAGE" "$WORK/build/terraform-provider-platformipam_v0.1.0"
if terraform init -input=false -lockfile=readonly -no-color > "$WORK/tampered.log" 2>&1; then
  echo "tampered provider package was accepted" >&2
  exit 1
fi
if ! rg -qi 'checksum|does not match|doesn.t match' "$WORK/tampered.log"; then
  cat "$WORK/tampered.log" >&2
  echo "tampered provider package failed for an unexpected reason" >&2
  exit 1
fi
echo "provider mirror: install, schema, readonly lock, and checksum rejection passed"
