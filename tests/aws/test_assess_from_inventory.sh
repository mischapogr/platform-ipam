#!/usr/bin/env bash
# True end-to-end test for the organization-inventory -> overlap-assessment
# pipeline (docs/WORK_PLAN.md package M1b4, ADR 0014): the REAL
# scripts/aws/org-inventory.sh, driven by a stubbed `aws` CLI exactly as its
# sibling tests/aws/test_org_inventory.sh drives it, writes an inventory
# directory, and the REAL `platform-ipam onboard assess` reads that directory
# and produces a report. Nobody had run this pipeline end to end before this
# package; internal/assess's own tests (TestCollectorFixtureScenario) and
# internal/onboardcmd's (assess_test.go) each replay one half of it from a
# hand-built fixture.
#
# Scenario 1 ("gapped"): two accounts whose VPCs overlap (an equal-cidr
# conflict), one region that is partial (its VPC call succeeded, its subnet
# call failed) and one account that cannot be assumed at all. Asserts the
# owner's sentence with its counts, both conflict sides naming networks.csv
# and the right row, association_id and observed_at populated on both sides
# (no association-id-missing/observation-time-missing limit -- this
# collector always writes both columns), coverage naming the partial region
# and the unassumable account, exit 3.
#
# Scenario 2 ("clean"): one account, one region, one VPC, nothing fails.
# Exit 0, coverage.complete true.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

# ORG_INVENTORY_SCRIPT and PLATFORM_IPAM_BIN are overridable so this test can
# run against a private working copy (docs/WORK_PLAN.md's isolation
# contract) and so CI or a developer with a locally built binary need not
# pay for a build through Docker. Both default to this
# checkout's own files, resolved from the test's OWN location -- never an
# absolute path into anybody's home directory.
ORG_INVENTORY_SCRIPT=${ORG_INVENTORY_SCRIPT:-$ROOT/scripts/aws/org-inventory.sh}
PLATFORM_IPAM_BIN=${PLATFORM_IPAM_BIN:-}

fail() { echo "FAIL: $*"; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "SKIP: $1 is not available"; exit 0; }
}
need jq
need bash

# run_assess invokes `platform-ipam onboard assess`. With PLATFORM_IPAM_BIN
# set it runs that binary directly. Otherwise the binary is built ONCE from
# this checkout through the pinned Go image -- there is no Go toolchain on the
# host this test can rely on -- and then run on the host like any other. It is
# built rather than started with `go run` inside the container for two reasons:
# a container does not see the inventory directory the collector wrote on the
# host, and `go run` answers 1 for every non-zero exit of the program it ran,
# which would hide exactly the exit statuses (0, 3, 4) this test asserts.
if [ -z "$PLATFORM_IPAM_BIN" ]; then
  need docker
  BUILD_DIR=$(mktemp -d)
  docker run --rm -u "$(id -u):$(id -g)" \
    -v "$ROOT":/src:ro -v "$BUILD_DIR":/out -v ipam-gomod:/go/pkg/mod \
    -w /src -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -e GOCACHE=/tmp/gocache -e HOME=/tmp \
    golang:1.26.8-bookworm go build -o /out/platform-ipam ./cmd/platform-ipam \
    || fail "could not build platform-ipam through the pinned Go image"
  PLATFORM_IPAM_BIN="$BUILD_DIR/platform-ipam"
fi

run_assess() {
  "$PLATFORM_IPAM_BIN" onboard assess "$@"
}

# ============================================================
# Scenario 1: two accounts overlap, one region is partial and
# populated, one account cannot be assumed.
# ============================================================

STUB1_DIR=$(mktemp -d)
OUT1_DIR=$(mktemp -d)

cat > "$STUB1_DIR/aws" << 'STUB_EOF'
#!/usr/bin/env bash
cmd="$*"

if [[ "$cmd" =~ organizations.*list-accounts ]]; then
  cat << 'JSON'
{
  "Accounts": [
    {"Id": "111111111111", "Name": "Alpha", "Status": "ACTIVE"},
    {"Id": "222222222222", "Name": "Beta", "Status": "ACTIVE"},
    {"Id": "333333333333", "Name": "Gamma", "Status": "ACTIVE"}
  ]
}
JSON
  exit 0
fi

if [[ "$cmd" =~ sts.*assume-role ]]; then
  if [[ "$cmd" =~ ::111111111111 ]]; then
    echo '{"Credentials": {"AccessKeyId": "AKIA111111111111AAAA", "SecretAccessKey": "fake-secret", "SessionToken": "fake-session-token"}}'
    exit 0
  elif [[ "$cmd" =~ ::222222222222 ]]; then
    echo '{"Credentials": {"AccessKeyId": "AKIA222222222222AAAA", "SecretAccessKey": "fake-secret", "SessionToken": "fake-session-token"}}'
    exit 0
  else
    exit 1  # account 333333333333: unassumable
  fi
fi

if [[ "$cmd" =~ sts.*get-caller-identity ]]; then
  if [[ "${AWS_ACCESS_KEY_ID:-}" =~ AKIA([0-9]+) ]]; then
    acct="${BASH_REMATCH[1]}"
  else
    acct="unknown"
  fi
  if [[ "$cmd" =~ --output.*text ]]; then
    echo "$acct"
  else
    echo "{\"Account\": \"$acct\"}"
  fi
  exit 0
fi

# describe-vpcs: account is distinguished by AWS_ACCESS_KEY_ID (REGIONS is
# configured explicitly, so both accounts are scanned in both regions).
if [[ "$cmd" =~ ec2.*describe-vpcs.*eu-central-1 ]]; then
  if [[ "${AWS_ACCESS_KEY_ID:-}" == AKIA111111111111AAAA ]]; then
    cat << 'JSON'
{"Vpcs":[{"VpcId":"vpc-alpha0000001","CidrBlock":"10.0.0.0/16","State":"available","Tags":[{"Key":"Name","Value":"alpha-vpc"}],
  "CidrBlockAssociationSet":[{"AssociationId":"vpc-cidr-assoc-alpha1","CidrBlock":"10.0.0.0/16","CidrBlockState":{"State":"associated"}}]}]}
JSON
  else
    cat << 'JSON'
{"Vpcs":[{"VpcId":"vpc-beta00000001","CidrBlock":"10.0.0.0/16","State":"available","Tags":[{"Key":"Name","Value":"beta-vpc"}],
  "CidrBlockAssociationSet":[{"AssociationId":"vpc-cidr-assoc-beta01","CidrBlock":"10.0.0.0/16","CidrBlockState":{"State":"associated"}}]}]}
JSON
  fi
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-vpcs.*us-east-1 ]]; then
  if [[ "${AWS_ACCESS_KEY_ID:-}" == AKIA111111111111AAAA ]]; then
    cat << 'JSON'
{"Vpcs":[{"VpcId":"vpc-alpha0000002","CidrBlock":"10.50.0.0/16","State":"available","Tags":[],
  "CidrBlockAssociationSet":[{"AssociationId":"vpc-cidr-assoc-alpha2","CidrBlock":"10.50.0.0/16","CidrBlockState":{"State":"associated"}}]}]}
JSON
  else
    echo '{"Vpcs":[]}'
  fi
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-subnets.*us-east-1 ]]; then
  if [[ "${AWS_ACCESS_KEY_ID:-}" == AKIA111111111111AAAA ]]; then
    echo "stub: subnet describe fails for account Alpha's us-east-1 (the planted partial region)" >&2
    exit 1
  fi
  echo '{"Subnets":[]}'
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-subnets.*eu-central-1 ]]; then
  echo '{"Subnets":[]}'
  exit 0
fi

echo "Unhandled: $cmd" >&2
exit 1
STUB_EOF
chmod +x "$STUB1_DIR/aws"

PATH="$STUB1_DIR:$PATH" ROLE_NAME=PlatformIpamReadOnly REGIONS="eu-central-1 us-east-1" OUT="$OUT1_DIR" \
  bash "$ORG_INVENTORY_SCRIPT"

echo "=== Scenario 1: collector output ==="
[ -f "$OUT1_DIR/networks.csv" ] || fail "networks.csv was not written"
[ -f "$OUT1_DIR/run.json" ] || fail "run.json was not written"
grep -q "10.0.0.0/16" "$OUT1_DIR/networks.csv" || fail "the overlapping CIDR is missing from networks.csv"
echo "✓ collector wrote networks.csv, failures.csv, accounts.json and run.json"

echo ""
echo "=== Scenario 1: onboard assess ==="
REPORT1=$(mktemp)
set +e
run_assess --inventory "$OUT1_DIR" --format json > "$REPORT1" 2> "$OUT1_DIR/assess-stderr.log"
code=$?
set -e
[ "$code" = 3 ] || { cat "$OUT1_DIR/assess-stderr.log"; fail "onboard assess exited $code, want 3"; }
echo "✓ onboard assess exits 3 (a report was produced and it is not clean)"

jq -e . "$REPORT1" >/dev/null || fail "report is not valid JSON"

[ "$(jq -r '.coverage.complete' "$REPORT1")" = "false" ] || fail "coverage.complete should be false"
echo "✓ coverage.complete is false"

[ "$(jq '.conflicts | length' "$REPORT1")" = "1" ] || { jq '.conflicts' "$REPORT1"; fail "expected exactly one conflict"; }
[ "$(jq -r '.conflicts[0].kind' "$REPORT1")" = "equal-cidr" ] || fail "expected an equal-cidr conflict"
echo "✓ exactly one equal-cidr conflict, between the two accounts' 10.0.0.0/16 VPCs"

# --- traceability: both sides name networks.csv, and their row is the real row ---
for i in 0 1; do
  side_file=$(jq -r ".conflicts[0].sides[$i].source_file" "$REPORT1")
  side_row=$(jq -r ".conflicts[0].sides[$i].source_row" "$REPORT1")
  side_cidr=$(jq -r ".conflicts[0].sides[$i].cidr" "$REPORT1")
  side_account=$(jq -r ".conflicts[0].sides[$i].account_id" "$REPORT1")
  case "$side_file" in
    */networks.csv) ;;
    *) fail "side $i source_file = $side_file, want it to end in networks.csv" ;;
  esac
  row_content=$(sed -n "${side_row}p" "$side_file")
  echo "$row_content" | grep -q "$side_account" || fail "side $i: row $side_row of $side_file does not contain account $side_account: $row_content"
  echo "$row_content" | grep -q "$side_cidr" || fail "side $i: row $side_row of $side_file does not contain CIDR $side_cidr: $row_content"

  assoc=$(jq -r ".conflicts[0].sides[$i].association_id" "$REPORT1")
  observed=$(jq -r ".conflicts[0].sides[$i].observed_at" "$REPORT1")
  # SC2015: `A && B || fail` is meant exactly as written -- fail runs when
  # either test is false, and fail exits, so C never runs after a true B.
  # shellcheck disable=SC2015
  [ "$assoc" != "null" ] && [ -n "$assoc" ] || fail "side $i: association_id is null, want the collector's own association id"
  # shellcheck disable=SC2015
  [ "$observed" != "null" ] && [ -n "$observed" ] || fail "side $i: observed_at is null, want the collector's own observed_at"
done
echo "✓ both conflict sides name networks.csv and the exact row they came from"
echo "✓ association_id and observed_at are populated on both sides (this collector always writes both columns)"

limits=$(jq -c '.input_limits' "$REPORT1")
echo "$limits" | grep -q "association-id-missing" && fail "input_limits should not carry association-id-missing: $limits"
echo "$limits" | grep -q "observation-time-missing" && fail "input_limits should not carry observation-time-missing: $limits"
echo "✓ input_limits carries neither association-id-missing nor observation-time-missing"

# --- coverage names the partial region and the unassumable account ---
jq -e '.coverage.partial[] | select(.account_id == "111111111111" and .region == "us-east-1")' "$REPORT1" >/dev/null \
  || { jq '.coverage.partial' "$REPORT1"; fail "coverage.partial does not name account 111111111111's us-east-1 region"; }
echo "✓ coverage.partial names the partial, populated region"

jq -e '.coverage.failed[] | select(.account_id == "333333333333")' "$REPORT1" >/dev/null \
  || { jq '.coverage.failed' "$REPORT1"; fail "coverage.failed does not name account 333333333333"; }
jq -e '.coverage.not_attempted[] | select(.account_id == "333333333333")' "$REPORT1" >/dev/null \
  || { jq '.coverage.not_attempted' "$REPORT1"; fail "coverage.not_attempted does not name account 333333333333"; }
echo "✓ coverage names the account that could not be assumed, both as a failure and as not attempted"

sentence=$(jq -r '.summary.sentence' "$REPORT1")
echo "$sentence" | grep -q "this report cannot make a complete statement for" \
  || fail "sentence does not use the incomplete template: $sentence"
echo "$sentence" | grep -qE "1 VPC/CIDR relationship" \
  || fail "sentence does not count the one relationship: $sentence"
for phrase in conflict-free "no conflicts" ready clean; do
  echo "$sentence" | grep -qi "$phrase" && fail "sentence contains the forbidden phrase '$phrase': $sentence"
done
echo "✓ the owner's sentence: \"$sentence\""

rm -rf "$STUB1_DIR" "$OUT1_DIR" "$REPORT1"

# ============================================================
# Scenario 2: one account, one region, one VPC, nothing fails.
# ============================================================

STUB2_DIR=$(mktemp -d)
OUT2_DIR=$(mktemp -d)

cat > "$STUB2_DIR/aws" << 'STUB_EOF'
#!/usr/bin/env bash
cmd="$*"
if [[ "$cmd" =~ organizations.*list-accounts ]]; then
  echo '{"Accounts": [{"Id": "444444444444", "Name": "Clean", "Status": "ACTIVE"}]}'
  exit 0
fi
if [[ "$cmd" =~ sts.*assume-role ]]; then
  echo '{"Credentials": {"AccessKeyId": "AKIA444444444444AAAA", "SecretAccessKey": "fake-secret", "SessionToken": "fake-session-token"}}'
  exit 0
fi
if [[ "$cmd" =~ sts.*get-caller-identity ]]; then
  if [[ "$cmd" =~ --output.*text ]]; then echo "444444444444"; else echo '{"Account":"444444444444"}'; fi
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-vpcs ]]; then
  cat << 'JSON'
{"Vpcs":[{"VpcId":"vpc-clean000001","CidrBlock":"10.90.0.0/16","State":"available","Tags":[],
  "CidrBlockAssociationSet":[{"AssociationId":"vpc-cidr-assoc-clean1","CidrBlock":"10.90.0.0/16","CidrBlockState":{"State":"associated"}}]}]}
JSON
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-subnets ]]; then
  echo '{"Subnets":[]}'
  exit 0
fi
echo "Unhandled: $cmd" >&2
exit 1
STUB_EOF
chmod +x "$STUB2_DIR/aws"

PATH="$STUB2_DIR:$PATH" ROLE_NAME=PlatformIpamReadOnly REGIONS="eu-central-1" OUT="$OUT2_DIR" \
  bash "$ORG_INVENTORY_SCRIPT"

echo ""
echo "=== Scenario 2: clean estate ==="
REPORT2=$(mktemp)
set +e
run_assess --inventory "$OUT2_DIR" --format json > "$REPORT2" 2> "$OUT2_DIR/assess-stderr.log"
code=$?
set -e
[ "$code" = 0 ] || { cat "$OUT2_DIR/assess-stderr.log"; fail "onboard assess exited $code, want 0"; }
[ "$(jq -r '.coverage.complete' "$REPORT2")" = "true" ] || fail "coverage.complete should be true"
[ "$(jq '.conflicts | length' "$REPORT2")" = "0" ] || fail "expected no conflicts"
echo "✓ onboard assess exits 0 with coverage.complete true and no conflicts"

# ============================================================
# Scenario 3 (package M1c): a truncated networks.csv beside an otherwise
# intact run.json from the SAME real collector run as Scenario 2 -- the
# exact hole docs/WORK_PLAN.md's M1c package names: "a truncated or
# hand-filtered networks file beside an intact run record still reports
# complete: true." Scenario 2's run.json records row_count 1 for account
# 444444444444's eu-central-1 (one VPC, no subnets); this truncates
# networks.csv down to its header alone (0 data rows) and re-runs assess
# with the intact accounts.json/failures.csv/run.json from the SAME run,
# so the only thing that changed is the rows onboard assess was actually
# handed -- exactly the truncated-or-hand-filtered-file scenario.
# ============================================================

echo ""
echo "=== Scenario 3: truncated networks.csv beside an intact run.json ==="
TRUNCATED_NETWORKS=$(mktemp)
head -n 1 "$OUT2_DIR/networks.csv" > "$TRUNCATED_NETWORKS"  # header only, 0 data rows
REPORT3=$(mktemp)
set +e
run_assess --networks "$TRUNCATED_NETWORKS" --accounts "$OUT2_DIR/accounts.json" \
  --failures "$OUT2_DIR/failures.csv" --run "$OUT2_DIR/run.json" --format json \
  > "$REPORT3" 2> "$OUT2_DIR/assess-stderr-3.log"
code=$?
set -e
[ "$code" = 3 ] || { cat "$OUT2_DIR/assess-stderr-3.log"; fail "onboard assess exited $code, want 3 (truncated networks.csv, intact run.json)"; }
echo "✓ onboard assess exits 3 (a report was produced and it is not clean)"

[ "$(jq -r '.coverage.complete' "$REPORT3")" = "false" ] || fail "coverage.complete should be false"
echo "✓ coverage.complete is false"

jq -e '.coverage.row_count_short[] | select(.account_id == "444444444444" and .region == "eu-central-1")' "$REPORT3" >/dev/null \
  || { jq '.coverage.row_count_short' "$REPORT3"; fail "coverage.row_count_short does not name account 444444444444's eu-central-1 region"; }
[ "$(jq -r '.coverage.row_count_short[0].recorded' "$REPORT3")" = "1" ] || fail "recorded row_count should be 1 (from run.json)"
[ "$(jq -r '.coverage.row_count_short[0].present' "$REPORT3")" = "0" ] || fail "present rows should be 0 (the truncated networks.csv)"
echo "✓ coverage.row_count_short names the account, region and both counts"

rm -f "$TRUNCATED_NETWORKS" "$REPORT3"

rm -rf "$STUB2_DIR" "$OUT2_DIR" "$REPORT2"

echo ""
echo "=========================================="
echo "All tests PASSED"
echo "=========================================="
