# Simulated operator beta

This is a local, disposable qualification of **Platform-IPAM-owned** address
pools. The input estate and its approval are synthetic. A passing allocation
check proves a durable local Platform-IPAM hold at the read instant. It does
not qualify AWS discovery, AWS-IPAM authority, customer acceptance, private
network connectivity, real identity providers or stage/production deployment.
`network_readiness` remains `NOT_ASSESSED`.

## Isolated stack and source

Run from the repository root. Record `git rev-parse HEAD` and `git status
--short` first. Use an application and provider built from that source, and
record both image/binary hashes in the evidence packet. Keep generated local
credentials private. `deploy/compose/.env` must have
`IPAM_ENVIRONMENT=development`, `IPAM_AUTH_MODE=local` and
`IPAM_AWS_MODE=fake`; if absent, create it from the repository root with
`deploy/compose/create-env.sh deploy/compose/.env`.
Do not point these commands at an existing Compose project.

```sh
export COMPOSE_PROJECT_NAME=platform-ipam-beta-e2e-$(date -u +%Y%m%d%H%M%S)
export IPAM_E2E_RUN_ID=beta-$(date -u +%Y%m%d%H%M%S)
work=$(mktemp -d /tmp/platform-ipam-beta.XXXXXX)
chmod 700 "$work"
compose() {
  docker compose --project-name "$COMPOSE_PROJECT_NAME" \
    --env-file deploy/compose/.env \
    -f deploy/compose/compose.yaml -f deploy/compose/compose.netbox.yaml "$@"
}
compose up --build -d netbox
```

Wait until `compose ps --format '{{.Service}} {{.Health}}'` reports NetBox
`healthy`. First database migration can take more than ten minutes; inspect
bounded `compose logs --tail=20 netbox` if progress stops. Then:

```sh
compose up --build -d
compose --profile bootstrap run --rm netbox-bootstrap
compose --profile bootstrap run --rm seed
compose ps

docker run --rm -v "$PWD:/src:ro" -v "$work:/out" \
  -v ipam-gomod:/go/pkg/mod -w /src golang:1.26.9-bookworm \
  go build -mod=readonly -buildvcs=false -o /out/platform-ipam ./cmd/platform-ipam
mkdir "$work/plugins"
docker run --rm -v "$PWD:/src:ro" -v "$work/plugins:/out" \
  -v ipam-gomod:/go/pkg/mod -w /src/providers/terraform \
  -e CGO_ENABLED=0 golang:1.26.9-bookworm \
  go build -mod=readonly -buildvcs=false -o /out/terraform-provider-platformipam .
```

## Inventory, conflict and review

Generate a fresh three-account input set before reserving anything in this
empty project. The generator uses the current binary's `onboard assess` and
`onboard progress` commands, then the address and pilot evidence scripts. It
also writes an `UNKNOWN_VARIANT` with one inaccessible account-region cell.
The generated `run.json` explicitly says `synthetic-fixture`; its recent
timestamps are for exercising freshness logic, not a claim of AWS collection.

```sh
key="beta/$(date -u +%Y%m%d%H%M%S)/primary"
python3 tests/e2e/generate-simulated-pilot.py \
  --binary "$work/platform-ipam" --key "$key" --out "$work/pilot"
```

Inspect `assessment.json`, `migration.yaml`, `address.json`,
`pilot-unreviewed.json`, `UNKNOWN_VARIANT/run.json` and
`unknown-address.json`. Require one confirmed `must_communicate` overlap, one
selected move, a `10.64.0.0/22` advisory candidate in `pool_dev_euc1`, and
no candidate when coverage is incomplete. The old overlapping CIDR stays
`CIDR_BLOCKED`. The generator refuses any other result.

Only after that inspection, complete `review-template.json` as `review.json`:
set `decision` to `approved`, name the synthetic reviewer in `reviewed_by`,
and set `reviewed_at` to the current UTC instant. Leave
`discovery.source=not_reviewed`; a synthetic scan cannot pass the live
discovery gate. Derive the reviewed report:

```sh
python3 scripts/aws/pilot-evidence.py \
  --address "$work/pilot/address.json" --progress "$work/pilot/progress.json" \
  --assessment "$work/pilot/assessment.json" --run "$work/pilot/run.json" \
  --networks "$work/pilot/networks.csv" --review "$work/pilot/review.json" \
  --out "$work/pilot/pilot-reviewed.json" >/dev/null
```

Require `input=PASS`, `planning=PASS`, `discovery=UNKNOWN` and
`allocation=UNKNOWN` before reservation. The input pass reflects this named
synthetic review alone.

## Terraform reservation and authority check

The example contains no AWS resource. The provider runs from the current
source through a local Terraform development override. Load only the local
development token into the shell; do not print or retain it in the evidence
packet.

```sh
mkdir "$work/tf"
cp examples/simulated-operator-beta/main.tf "$work/tf/main.tf"
cat > "$work/terraform.rc" <<'EOF'
provider_installation {
  dev_overrides {
    "registry.example.com/platform/platformipam" = "/plugins"
  }
  direct {}
}
EOF
selected_project=$COMPOSE_PROJECT_NAME
set -a
. deploy/compose/.env
set +a
COMPOSE_PROJECT_NAME=$selected_project
export COMPOSE_PROJECT_NAME IPAM_E2E_RUN_ID
export PLATFORM_IPAM_TOKEN="$IPAM_LOCAL_TOKEN"
export PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1
api_container=$(compose ps -q api)
allocations_snapshot() {
  python3 - "$1" <<'PY'
import json, os, sys, urllib.request
from pathlib import Path
request = urllib.request.Request('http://127.0.0.1:8080/v1/allocations',
    headers={'Authorization': 'Bearer ' + os.environ['IPAM_OPERATOR_TOKEN']})
with urllib.request.urlopen(request, timeout=30) as response:
    Path(sys.argv[1]).write_text(json.dumps(json.load(response)) + '\n')
PY
}
terraform_local() {
  docker run --rm --network "container:$api_container" \
    -v "$work/tf:/work" -v "$work/plugins:/plugins:ro" \
    -v "$work/terraform.rc:/terraform.rc:ro" -w /work \
    -e TF_CLI_CONFIG_FILE=/terraform.rc -e TF_IN_AUTOMATION=1 \
    -e PLATFORM_IPAM_TOKEN -e PLATFORM_IPAM_ALLOW_LOCAL_HTTP \
    --entrypoint terraform hashicorp/terraform:1.14 "$@"
}
allocations_snapshot "$work/tf/before-plan.json"
terraform_local plan -input=false -var="allocation_key=$key" -out=pilot.tfplan
allocations_snapshot "$work/tf/after-plan.json"
python3 - "$work/tf/before-plan.json" "$work/tf/after-plan.json" <<'PY'
import json, sys
assert all(not json.load(open(path))['items'] for path in sys.argv[1:])
PY
terraform_local show -json pilot.tfplan > "$work/tf/plan.json"
python3 providers/terraform/scripts/check-replacement-plan.py \
  --plan-json "$work/tf/plan.json"
terraform_local apply -input=false -auto-approve pilot.tfplan
terraform_local output -json > "$work/tf/outputs.json"
terraform_local plan -input=false -detailed-exitcode -var="allocation_key=$key"
python3 - "$work/tf/outputs.json" "$key" <<'PY'
import json, sys
sys.path.insert(0, 'tests/e2e')
from harness import stack
outputs = json.load(open(sys.argv[1]))
cidr = outputs['cidr']['value']
allocation_id = outputs['allocation_id']['value']
prefix = stack().netbox_prefixes()[cidr]
fields = prefix['custom_fields']
assert fields['platform_allocation_id'] == allocation_id
assert fields['platform_allocation_key'] == sys.argv[2]
PY
```

Before apply, an authenticated allocation list must be empty; the plan must
leave it empty and show the CIDR as unknown. Apply must return the reviewed
`10.64.0.0/22`; the final normal plan must exit 0 with no changes. Check
that NetBox's managed Prefix has the same CIDR, allocation ID and key.

Use the read-only operator token for authority verification. It must report
one `VERIFIED` reservation with `network_readiness=NOT_ASSESSED`:

```sh
PLATFORM_IPAM_TOKEN="$IPAM_OPERATOR_TOKEN" \
python3 scripts/aws/verify-reservations.py \
  --pilot "$work/pilot/pilot-reviewed.json" \
  --approved-plan "$work/pilot/approved-plan.json" \
  --protected "$work/pilot/protected.json" \
  --networks "$work/pilot/networks.csv" --run "$work/pilot/run.json" \
  --url http://127.0.0.1:8080 --max-inventory-age-seconds 600 \
  --out "$work/pilot/verification.json" >/dev/null
python3 scripts/aws/pilot-evidence.py \
  --address "$work/pilot/address.json" --progress "$work/pilot/progress.json" \
  --assessment "$work/pilot/assessment.json" --run "$work/pilot/run.json" \
  --networks "$work/pilot/networks.csv" --review "$work/pilot/review.json" \
  --verification "$work/pilot/verification.json" \
  --out "$work/pilot/pilot-verified.json" >/dev/null
```

The verified report's allocation gate can pass while its discovery gate
remains `UNKNOWN`. Never label the four-gate customer pilot as passed.

## Refusals, regression and cleanup

Run the verifier again with `--max-inventory-age-seconds 1`: it must exit 4
with an inventory-stale diagnostic. A forced Terraform replacement with the
same key must be rejected by the saved-plan guard; **do not apply it**:

```sh
terraform_local plan -input=false -var="allocation_key=$key" \
  -replace=platformipam_allocation.replacement -out=unsafe.tfplan
terraform_local show -json unsafe.tfplan > "$work/tf/unsafe-plan.json"
python3 providers/terraform/scripts/check-replacement-plan.py \
  --plan-json "$work/tf/unsafe-plan.json"
```

The last command must exit 1 and name `platformipam_allocation.replacement`.
The end-to-end suite additionally tests wrong identity, imported overlap,
stable-key replay, release quarantine and CIDR non-reuse. Capture the pilot
verification first: the suite intentionally resets this disposable project's
allocation state.

```sh
IPAM_E2E_PATTERN='test_e2e_[a-i]*.py' ./tests/e2e/run-e2e.sh
IPAM_E2E_SKIP_PREPARE=1 IPAM_E2E_PATTERN='test_e2e_[j-z]*.py' ./tests/e2e/run-e2e.sh
```

Require both halves to pass with **zero skips**. Run `scripts/ai/check-contract`,
`scripts/ai/check-compose`, `scripts/ai/check-provider` and the offline
`PLATFORM_IPAM_BIN="$work/platform-ipam" scripts/ai/check-aws` separately;
these checks do not substitute for the runtime suite. Record their compact
results and log paths, the exact input/report digests, source commit, binary
and image identity, and any skipped or failed step. Then remove only this
project's containers and volumes:

```sh
compose down --volumes --remove-orphans
```

Retain a dated, credential-free evidence packet if the run is being used to
qualify a candidate. The temporary Terraform state and generated local
credentials are not release artifacts.

The [2026-10-26 qualification packet](qualification/SIMULATED_OPERATOR_BETA_2026_10_26.json)
records one complete run on source commit `06d8e01`, with the exact synthetic
inputs alongside it. Its reservation verification has expired and is retained
as historical evidence of the read instant, not a current hold assertion.

The [second-operator repeat packet](qualification/SIMULATED_OPERATOR_BETA_REPEAT_2026_10_26.json)
records a fresh detached clone of the same commit, the explicit hashes of the
uncommitted workflow files copied into it, another full local run, transient
setup failures and their resolutions, timings, and cleanup. It was performed
by Codex acting as a second operator; it is not independent human acceptance.
