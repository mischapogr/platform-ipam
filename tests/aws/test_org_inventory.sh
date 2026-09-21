#!/usr/bin/env bash
# Test for scripts/aws/org-inventory.sh
# Creates stub aws CLIs that return canned responses and verifies script output:
# networks.csv, failures.csv and run.json (ADR 0014, work-plan package M1b1).

set -euo pipefail

# SCRIPT is overridable so this test can run against a private working copy
# (docs/WORK_PLAN.md M1b1's isolation contract); the default is this checkout's.
SCRIPT=${SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/aws/org-inventory.sh}

RFC3339='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'

fail() { echo "FAIL: $*"; exit 1; }

# assert_no_credential_strings scans every file in a directory for anything
# that looks like a credential this run touched: the stub's fake access key
# id/secret/session token literals, or the AKIA prefix real AWS access keys
# always start with. failures.csv is allowed to carry the identity-mismatch
# account id (pre-existing behaviour, unchanged by this package); run.json
# must never carry it (checked separately, per scenario).
assert_no_credential_strings() {
  local dir=$1
  local leaked
  if leaked=$(grep -rlE 'AKIA[A-Z0-9]+|SecretAccessKey|SessionToken|fake-secret|fake-session-token' "$dir" 2>/dev/null); then
    fail "a credential-looking string was found in: $leaked"
  fi
}

# --- Scenario 1: the original fixture, extended with AssociationId, plus
# every backward-compatibility assertion the original test made. ---

STUB1_DIR=$(mktemp -d)
OUT1_DIR=$(mktemp -d)

cat > "$STUB1_DIR/aws" << 'STUB_EOF'
#!/usr/bin/env bash
cmd="$*"

if [[ "$cmd" =~ organizations.*list-accounts ]]; then
  cat << 'JSON'
{
  "Accounts": [
    {"Id": "111111111111", "Name": "Alpha, Prod", "Status": "ACTIVE"},
    {"Id": "222222222222", "Name": "Beta", "Status": "ACTIVE"},
    {"Id": "333333333333", "Name": "Gamma, Suspended", "Status": "SUSPENDED"}
  ]
}
JSON
  exit 0
fi

if [[ "$cmd" =~ sts.*assume-role ]]; then
  if [[ "$cmd" =~ ::111111111111 ]]; then
    cat << 'JSON'
{"Credentials": {"AccessKeyId": "AKIA111111111111AAAA", "SecretAccessKey": "fake-secret", "SessionToken": "fake-session-token"}}
JSON
    exit 0
  else
    exit 1
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
    cat << JSON
{"Account": "$acct"}
JSON
  fi
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-regions ]]; then
  if [[ "$cmd" =~ --output.*text ]]; then
    echo "eu-central-1"
  else
    cat << 'JSON'
{"Regions": [{"RegionName": "eu-central-1"}]}
JSON
  fi
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-vpcs ]]; then
  cat << 'JSON'
{
  "Vpcs": [
    {
      "VpcId": "vpc-12345678",
      "CidrBlock": "10.0.0.0/16",
      "State": "available",
      "Tags": [{"Key": "Name", "Value": "My, VPC"}],
      "CidrBlockAssociationSet": [
        {"AssociationId": "vpc-cidr-assoc-primary1", "CidrBlock": "10.0.0.0/16", "CidrBlockState": {"State": "associated"}},
        {"AssociationId": "vpc-cidr-assoc-secondary1", "CidrBlock": "10.1.0.0/16", "CidrBlockState": {"State": "associated"}},
        {"AssociationId": "vpc-cidr-assoc-disassoc1", "CidrBlock": "192.168.0.0/16", "CidrBlockState": {"State": "disassociated"}}
      ]
    }
  ]
}
JSON
  exit 0
fi

if [[ "$cmd" =~ ec2.*describe-subnets ]]; then
  cat << 'JSON'
{
  "Subnets": [
    {
      "SubnetId": "subnet-87654321",
      "VpcId": "vpc-12345678",
      "CidrBlock": "10.0.1.0/24",
      "AvailabilityZoneId": "euc1-az1",
      "State": "available",
      "Tags": [{"Key": "Name", "Value": "My Subnet"}]
    }
  ]
}
JSON
  exit 0
fi

echo "Unhandled: $cmd" >&2
exit 1
STUB_EOF
chmod +x "$STUB1_DIR/aws"

PATH="$STUB1_DIR:$PATH" ROLE_NAME=PlatformIpamReadOnly REGIONS=eu-central-1 OUT="$OUT1_DIR" \
  bash "$SCRIPT"

echo "=== Scenario 1: networks.csv ==="
networks_file="$OUT1_DIR/networks.csv"

header=$(head -1 "$networks_file")
expected="account_id,account_name,region,type,resource_id,cidr,parent_id,az_id,name,state,primary,association_id,observed_at"
[ "$header" = "$expected" ] || fail "header mismatch: $header"
echo "✓ networks.csv header carries association_id and observed_at, appended after the existing columns"

line_count=$(wc -l < "$networks_file")
[ "$line_count" = "4" ] || { cat "$networks_file"; fail "expected 4 lines (header + 3 rows), got $line_count"; }
echo "✓ networks.csv has correct header and 3 data rows"

vpc_count=$(grep -c ',"vpc",' "$networks_file" || true)
subnet_count=$(grep -c ',"subnet",' "$networks_file" || true)
[ "$vpc_count" -eq 2 ] || fail "expected 2 VPC rows, got $vpc_count"
[ "$subnet_count" -eq 1 ] || fail "expected 1 subnet row, got $subnet_count"
echo "✓ Found 2 VPCs and 1 subnet"

if grep -q "192.168.0.0/16" "$networks_file"; then
  fail "disassociated CIDR should be absent"
fi
echo "✓ Disassociated CIDR correctly filtered"

if grep -q '"My, VPC"' "$networks_file"; then
  echo "✓ Name with comma correctly quoted"
else
  fail "name not properly quoted"
fi

# association_id: present (non-empty) and distinct on the primary and
# secondary VPC rows, absent (empty) on the subnet row. Row order matches
# scan_region's own emission order (primary association first).
primary_row=$(sed -n '2p' "$networks_file")
secondary_row=$(sed -n '3p' "$networks_file")
subnet_row=$(sed -n '4p' "$networks_file")

# A plain "cut -d',' -fN"/"awk -F','" field count is unsafe here: several
# columns (account_name "Alpha, Prod", the VPC's name "My, VPC") embed a
# comma inside their own quotes, which shifts naive positional field
# indexing. These checks instead anchor on substrings and, for the
# empty-vs-non-empty distinction, on the unambiguous end of the line.
echo "$primary_row" | grep -q '"vpc-cidr-assoc-primary1"' || fail "primary row missing its association_id: $primary_row"
echo "$secondary_row" | grep -q '"vpc-cidr-assoc-secondary1"' || fail "secondary row missing its association_id: $secondary_row"
echo "$primary_row" | grep -q '"vpc-cidr-assoc-secondary1"' && fail "primary row must not also carry the secondary association id"
echo "$secondary_row" | grep -q '"vpc-cidr-assoc-primary1"' && fail "secondary row must not also carry the primary association id"
echo "✓ association_id set, and distinct, on the primary and secondary VPC associations"

# The subnet row's trailing three columns are state, primary (empty for a
# subnet) and association_id (always empty for a subnet): matched here as
# ...,"available","","",<observed_at>$ so the check is anchored at the end
# of the line and unaffected by any embedded comma earlier in the row.
echo "$subnet_row" | grep -qE '"available","","","[0-9TZ:-]+"$' \
  || fail "subnet row must have primary and association_id both empty, ending in ...,\"available\",\"\",\"\",<observed_at>: $subnet_row"
echo "✓ association_id is empty on the subnet row"

# observed_at: present and RFC 3339 on every row (the trailing column, field 13).
for row_num in 2 3 4; do
  observed=$(sed -n "${row_num}p" "$networks_file" | awk -F',' '{print $NF}' | tr -d '"')
  echo "$observed" | grep -qE "$RFC3339" || fail "row $row_num observed_at is not RFC 3339: $observed"
done
echo "✓ observed_at is present and RFC 3339 on every row"

echo ""
echo "=== Scenario 1: failures.csv ==="
failures_file="$OUT1_DIR/failures.csv"

header=$(head -1 "$failures_file")
expected="account_id,account_name,region,stage,error"
[ "$header" = "$expected" ] || fail "failures header mismatch"

line_count=$(wc -l < "$failures_file")
[ "$line_count" = "2" ] || { cat "$failures_file"; fail "expected 2 lines (header + 1 failure), got $line_count"; }
echo "✓ failures.csv has header + 1 failure row"

grep -q '^222222222222,' "$failures_file" || fail "missing 222 failure"
expected_failures='account_id,account_name,region,stage,error
222222222222,"Beta",,assume-role,failed'
if [ "$(cat "$failures_file")" != "$expected_failures" ]; then
  diff <(printf '%s\n' "$expected_failures") "$failures_file" || true
  fail "failures.csv differs from the expected content"
fi
echo "✓ Failure recorded for account 222222222222, failures.csv column set unchanged"

grep -q '333333333333' "$failures_file" && fail "suspended account should not appear"
echo "✓ Suspended account correctly excluded"

echo ""
echo "=== Scenario 1: run.json (fully successful run) ==="
run_file="$OUT1_DIR/run.json"
[ -f "$run_file" ] || fail "run.json was not written"
jq -e . "$run_file" >/dev/null || fail "run.json is not valid JSON"

[ "$(jq -r '.role_name' "$run_file")" = "PlatformIpamReadOnly" ] || fail "run.json role_name wrong"
[ "$(jq -r '.management_account_used' "$run_file")" = "false" ] || fail "run.json management_account_used wrong"
[ "$(jq -r '.management_account_id' "$run_file")" = "null" ] || fail "run.json management_account_id should be null"
[ "$(jq -r '.configured_regions | length' "$run_file")" = "1" ] || fail "run.json configured_regions wrong"
jq -r '.started_at' "$run_file" | grep -qE "$RFC3339" || fail "run.json started_at not RFC 3339"
jq -r '.finished_at' "$run_file" | grep -qE "$RFC3339" || fail "run.json finished_at not RFC 3339"

acct111=$(jq -c '.accounts[] | select(.account_id=="111111111111")' "$run_file")
[ -n "$acct111" ] || fail "run.json missing account 111111111111"
echo "$acct111" | jq -e '.regions_attempted == "known"' >/dev/null || fail "111111111111 regions_attempted should be known"
echo "$acct111" | jq -e '.not_attempted_reason == null' >/dev/null || fail "111111111111 not_attempted_reason should be null"
echo "$acct111" | jq -e '.regions | length == 1' >/dev/null || fail "111111111111 should have exactly one region entry"
echo "$acct111" | jq -e '.regions[0].region == "eu-central-1"' >/dev/null || fail "111111111111 region name wrong"
echo "$acct111" | jq -e '.regions[0].outcome == "succeeded"' >/dev/null || fail "111111111111 region outcome should be succeeded"
echo "$acct111" | jq -e '.regions[0].row_count == 3' >/dev/null || fail "111111111111 row_count should be 3 (2 vpc + 1 subnet)"
echo "$acct111" | jq -e '.regions[0].observed_at | test("'"$RFC3339"'")' >/dev/null || fail "111111111111 region observed_at not RFC 3339"
echo "✓ run.json: account 111111111111 recorded succeeded with row_count 3"

acct222=$(jq -c '.accounts[] | select(.account_id=="222222222222")' "$run_file")
[ -n "$acct222" ] || fail "run.json missing account 222222222222"
echo "$acct222" | jq -e '.not_attempted_reason == "assume-role-failed"' >/dev/null || fail "222222222222 not_attempted_reason wrong"
echo "$acct222" | jq -e '.regions | length == 1' >/dev/null || fail "222222222222 should list its one configured region as not_attempted"
echo "$acct222" | jq -e '.regions[0].outcome == "not_attempted"' >/dev/null || fail "222222222222 region outcome wrong"
echo "✓ run.json: account 222222222222 (could not be assumed) recorded not_attempted, reason assume-role-failed"

jq -e '.accounts | length == 2' "$run_file" >/dev/null || fail "run.json must not mention the suspended account"
echo "✓ run.json: exactly the two active accounts, suspended account absent"

assert_no_credential_strings "$OUT1_DIR"
grep -q '999999999999' "$run_file" 2>/dev/null && fail "run.json must never carry an identity-mismatch account id"
echo "✓ no credential-looking string in any output file (scenario 1)"

rm -rf "$STUB1_DIR" "$OUT1_DIR"

# --- Scenario 2: one region partial (VPC succeeds, subnet describe fails,
# VPC rows still land in networks.csv) and one region fully failed
# (describe-vpcs itself fails, zero rows for that region). ---

STUB2_DIR=$(mktemp -d)
OUT2_DIR=$(mktemp -d)

cat > "$STUB2_DIR/aws" << 'STUB_EOF'
#!/usr/bin/env bash
cmd="$*"
if [[ "$cmd" =~ organizations.*list-accounts ]]; then
  echo '{"Accounts": [{"Id": "444444444444", "Name": "Partial", "Status": "ACTIVE"}]}'
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
if [[ "$cmd" =~ ec2.*describe-vpcs.*eu-central-1 ]]; then
  cat << 'JSON'
{"Vpcs":[{"VpcId":"vpc-partial1","CidrBlock":"10.20.0.0/16","State":"available","Tags":[],
  "CidrBlockAssociationSet":[{"AssociationId":"vpc-cidr-assoc-partial1","CidrBlock":"10.20.0.0/16","CidrBlockState":{"State":"associated"}}]}]}
JSON
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-subnets.*eu-central-1 ]]; then
  echo "stub: subnet describe fails for this region" >&2
  exit 1
fi
if [[ "$cmd" =~ ec2.*describe-vpcs.*us-east-1 ]]; then
  echo "stub: vpc describe fails for this region" >&2
  exit 1
fi
echo "Unhandled: $cmd" >&2
exit 1
STUB_EOF
chmod +x "$STUB2_DIR/aws"

PATH="$STUB2_DIR:$PATH" ROLE_NAME=PlatformIpamReadOnly REGIONS="eu-central-1 us-east-1" OUT="$OUT2_DIR" \
  bash "$SCRIPT"

echo ""
echo "=== Scenario 2: partial region keeps its VPC rows, failed region has none ==="
grep -q "10.20.0.0/16" "$OUT2_DIR/networks.csv" || fail "partial region's VPC row must still be written"
echo "✓ networks.csv keeps the partial region's VPC row"

failures2=$(cat "$OUT2_DIR/failures.csv")
expected_failures2='account_id,account_name,region,stage,error
444444444444,"Partial",eu-central-1,describe,failed
444444444444,"Partial",us-east-1,describe,failed'
[ "$failures2" = "$expected_failures2" ] || { diff <(printf '%s\n' "$expected_failures2") "$OUT2_DIR/failures.csv" || true; fail "failures.csv (scenario 2) differs from expected"; }
echo "✓ failures.csv carries one describe-stage row for each of the two failed regions"

run2="$OUT2_DIR/run.json"
partial_entry=$(jq -c '.accounts[0].regions[] | select(.region=="eu-central-1")' "$run2")
[ "$(echo "$partial_entry" | jq -r .outcome)" = "partial" ] || fail "eu-central-1 outcome should be partial: $partial_entry"
[ "$(echo "$partial_entry" | jq -r .stage)" = "describe" ] || fail "eu-central-1 stage should be describe"
[ "$(echo "$partial_entry" | jq -r .row_count)" = "1" ] || fail "eu-central-1 row_count should be 1 (the VPC row that survived)"
echo "✓ run.json: eu-central-1 recorded partial with row_count 1"

failed_entry=$(jq -c '.accounts[0].regions[] | select(.region=="us-east-1")' "$run2")
[ "$(echo "$failed_entry" | jq -r .outcome)" = "failed" ] || fail "us-east-1 outcome should be failed: $failed_entry"
[ "$(echo "$failed_entry" | jq -r .stage)" = "describe" ] || fail "us-east-1 stage should be describe"
echo "✓ run.json: us-east-1 (describe-vpcs itself failed) recorded failed, no row_count"

assert_no_credential_strings "$OUT2_DIR"
echo "✓ no credential-looking string in any output file (scenario 2)"

rm -rf "$STUB2_DIR" "$OUT2_DIR"

# --- Scenario 3: one region read successfully and EMPTY (zero VPCs, zero
# subnets) -- the "read and empty" case run.json exists to make sayable --
# scanned with management-account credentials and no REGIONS configured
# (region list discovered per account). ---

STUB3_DIR=$(mktemp -d)
OUT3_DIR=$(mktemp -d)

cat > "$STUB3_DIR/aws" << 'STUB_EOF'
#!/usr/bin/env bash
cmd="$*"
if [[ "$cmd" =~ organizations.*list-accounts ]]; then
  echo '{"Accounts": [{"Id": "666666666666", "Name": "Mgmt", "Status": "ACTIVE"}]}'
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-regions ]]; then
  if [[ "$cmd" =~ --output.*text ]]; then echo "eu-central-1"; else echo '{"Regions":[{"RegionName":"eu-central-1"}]}'; fi
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-vpcs ]]; then
  echo '{"Vpcs":[]}'
  exit 0
fi
if [[ "$cmd" =~ ec2.*describe-subnets ]]; then
  echo '{"Subnets":[]}'
  exit 0
fi
echo "Unhandled: $cmd" >&2
exit 1
STUB_EOF
chmod +x "$STUB3_DIR/aws"

PATH="$STUB3_DIR:$PATH" ROLE_NAME=PlatformIpamReadOnly REGIONS="" MANAGEMENT_ACCOUNT_ID=666666666666 OUT="$OUT3_DIR" \
  bash "$SCRIPT"

echo ""
echo "=== Scenario 3: region read successfully and empty ==="
line_count3=$(wc -l < "$OUT3_DIR/networks.csv")
[ "$line_count3" = "1" ] || fail "networks.csv should have only the header for an empty read, got $line_count3 lines"
echo "✓ networks.csv has no data rows for a region with zero VPCs and zero subnets"

[ ! -s "$OUT3_DIR/failures.csv" ] || [ "$(wc -l < "$OUT3_DIR/failures.csv")" = "1" ] || fail "failures.csv should be header-only"
echo "✓ failures.csv is header-only: an empty read is not a failure"

run3="$OUT3_DIR/run.json"
[ "$(jq -r '.management_account_used' "$run3")" = "true" ] || fail "run.json management_account_used should be true"
[ "$(jq -r '.management_account_id' "$run3")" = "666666666666" ] || fail "run.json management_account_id wrong"
[ "$(jq -r '.configured_regions' "$run3")" = "null" ] || fail "run.json configured_regions should be null when REGIONS is unset"

empty_entry=$(jq -c '.accounts[0].regions[0]' "$run3")
[ "$(echo "$empty_entry" | jq -r .outcome)" = "succeeded" ] || fail "empty region outcome should be succeeded, not absent or failed"
[ "$(echo "$empty_entry" | jq -r .row_count)" = "0" ] || fail "empty region row_count should be 0"
echo "✓ run.json distinguishes read-and-empty (succeeded, row_count 0) from not-read"

assert_no_credential_strings "$OUT3_DIR"
echo "✓ no credential-looking string in any output file (scenario 3)"

rm -rf "$STUB3_DIR" "$OUT3_DIR"

echo ""
echo "=========================================="
echo "All tests PASSED"
echo "=========================================="
