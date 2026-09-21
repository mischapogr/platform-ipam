#!/usr/bin/env bash
# Inventories AWS accounts, networks and IP ranges from an organization.
# Scans every active account via assumed role or management account credentials.
# Outputs networks.csv, failures.csv, accounts.json and run.json to OUT directory.
#
# Usage: ROLE_NAME=PlatformIpamReadOnly REGIONS="eu-central-1 us-east-1" OUT=inventory ./org-inventory.sh
#
# run.json (docs/AWS_ORGANIZATION_INVENTORY.md, ADR 0014): records, for every
# ACTIVE account and every region attempted for it, whether that account and
# region was read successfully (with a row count, so "read and empty" can be
# told from "not read"), partially read (the VPC call succeeded, the subnet
# call failed, and the VPC rows still landed in networks.csv), failed, or --
# when the account could not be assumed, or its region list could not be
# obtained -- not attempted at all. It carries no credential, session token,
# or account id beyond what accounts.json already holds.

set -euo pipefail

# SCRIPT_VERSION is written into run.json's script_version field. Bumped from
# the implicit "1" (before association_id, observed_at and run.json existed)
# because run.json's own shape is part of what a consumer needs to know.
SCRIPT_VERSION="2"

ROLE_NAME=${ROLE_NAME:-PlatformIpamReadOnly}
OUT=${OUT:-inventory}
# Leave REGIONS empty to scan every region enabled in each account.
REGIONS=${REGIONS:-}
# Optional: scan the management account with its own credentials instead of assume-role
MANAGEMENT_ACCOUNT_ID=${MANAGEMENT_ACCOUNT_ID:-}

mkdir -p "$OUT"
hdr='account_id,account_name,region,type,resource_id,cidr,parent_id,az_id,name,state,primary,association_id,observed_at'
echo "$hdr" > "$OUT/networks.csv"
echo 'account_id,account_name,region,stage,error' > "$OUT/failures.csv"

# run.json is assembled from per-account fragments rather than built up in a
# shell variable: the account loop below pipes accounts.json through jq into
# a `while read`, and both sides of a pipe run in a subshell in bash, so any
# variable the loop body set would be lost the moment the loop ends. Each
# account subshell instead appends exactly one JSON object (one line) to
# accounts.ndjson, and the fragments are slurped into run.json only after the
# loop has completed. That also means an interrupted run (Ctrl-C, a killed
# shell) leaves these fragment files behind but writes no run.json at all --
# run.json's own absence is how a downstream reader (ADR 0014's assessment)
# tells an interrupted run from a completed one. The EXIT trap below cleans
# the fragments up on any signal bash can trap; it cannot run after SIGKILL,
# which is the one case a stray fragment directory can be left on disk (never
# a stray or partial run.json, since that file is written in one jq call at
# the very end).
RUN_FRAGMENTS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/org-inventory-run.XXXXXX")
trap 'rm -rf "$RUN_FRAGMENTS_DIR"' EXIT
: > "$RUN_FRAGMENTS_DIR/accounts.ndjson"

started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# record_account_unattempted account name credential_source reason attempted_set
#   attempted_set is "known" when REGIONS was configured -- every configured
#   region is then listed as not_attempted, because the intended set really
#   is known even though nothing in it was reached -- or "unknown" when no
#   region list was ever obtained for this account, in which case nothing is
#   invented and regions stays an empty list.
record_account_unattempted() {
  local a=$1 n=$2 cred=$3 reason=$4 attempted=$5
  if [ "$attempted" = known ]; then
    local region_objs=() r
    for r in $REGIONS; do
      region_objs+=("$(jq -nc --arg r "$r" --arg reason "$reason" \
        '{region:$r, outcome:"not_attempted", reason:$reason}')")
    done
    local regions_json
    regions_json=$(printf '%s\n' "${region_objs[@]}" | jq -s 'sort_by(.region)')
    jq -n --arg a "$a" --arg n "$n" --arg cred "$cred" --arg reason "$reason" --argjson regions "$regions_json" '
      {account_id:$a, account_name:$n, credential_source:$cred, regions_attempted:"known",
       not_attempted_reason:$reason, region_source:"configured", regions:$regions}' \
      >> "$RUN_FRAGMENTS_DIR/accounts.ndjson"
  else
    jq -n --arg a "$a" --arg n "$n" --arg cred "$cred" --arg reason "$reason" '
      {account_id:$a, account_name:$n, credential_source:$cred, regions_attempted:"unknown",
       not_attempted_reason:$reason, region_source:"discovered", regions: []}' \
      >> "$RUN_FRAGMENTS_DIR/accounts.ndjson"
  fi
}

scan_region() {  # account name region
  local a=$1 n=$2 r=$3
  # One instant per region scan, taken before the first describe call: the
  # region's rows (VPC and, when it succeeds, subnet) are what that one read
  # of the region produced, and a single shared instant is simpler and more
  # honest than two, which would imply a precision (the gap between the VPC
  # and subnet calls) nobody asked for and ADR 0014 does not require.
  local observed_at; observed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  REGION_OBSERVED_AT=$observed_at

  local vpc_tmp; vpc_tmp=$(mktemp "$RUN_FRAGMENTS_DIR/vpc.XXXXXX")
  if ! aws ec2 describe-vpcs --region "$r" --output json | jq -r --arg a "$a" --arg n "$n" --arg r "$r" --arg observed "$observed_at" '
    .Vpcs[] | . as $v
    | (($v.Tags // []) | map(select(.Key=="Name")) | .[0].Value // "") as $name
    | ($v.CidrBlockAssociationSet // [])[]
    | select(.CidrBlockState.State=="associated")
    | [$a,$n,$r,"vpc",$v.VpcId,.CidrBlock,"","",$name,$v.State,(.CidrBlock==$v.CidrBlock),.AssociationId,$observed] | @csv' \
    > "$vpc_tmp"; then
    rm -f "$vpc_tmp"
    REGION_OUTCOME=failed
    REGION_ROW_COUNT=0
    return 1   # inside an "if !" context set -e is off, so fail explicitly
  fi
  cat "$vpc_tmp" >> "$OUT/networks.csv"
  local vpc_rows; vpc_rows=$(wc -l < "$vpc_tmp")
  rm -f "$vpc_tmp"

  local sub_tmp; sub_tmp=$(mktemp "$RUN_FRAGMENTS_DIR/sub.XXXXXX")
  if ! aws ec2 describe-subnets --region "$r" --output json | jq -r --arg a "$a" --arg n "$n" --arg r "$r" --arg observed "$observed_at" '
    .Subnets[]
    | ((.Tags // []) | map(select(.Key=="Name")) | .[0].Value // "") as $name
    | [$a,$n,$r,"subnet",.SubnetId,.CidrBlock,.VpcId,.AvailabilityZoneId,$name,.State,"","",$observed] | @csv' \
    > "$sub_tmp"; then
    rm -f "$sub_tmp"
    REGION_OUTCOME=partial
    REGION_ROW_COUNT=$vpc_rows
    return 1
  fi
  cat "$sub_tmp" >> "$OUT/networks.csv"
  local sub_rows; sub_rows=$(wc -l < "$sub_tmp")
  rm -f "$sub_tmp"

  REGION_OUTCOME=succeeded
  REGION_ROW_COUNT=$((vpc_rows + sub_rows))
  return 0
}

scan_account() {  # account name credential_source   (runs with that account's credentials in the environment)
  local a=$1 n=$2 cred=$3 regions=$REGIONS region_source=configured
  if [ -z "$regions" ]; then
    region_source=discovered
    if ! regions=$(aws ec2 describe-regions --region us-east-1 --query 'Regions[].RegionName' --output text); then
      echo "$a,\"$n\",,describe-regions,failed" >> "$OUT/failures.csv"
      record_account_unattempted "$a" "$n" "$cred" "region-list-unavailable" unknown
      return
    fi
  fi

  local region_objs=() r
  for r in $regions; do
    scan_region "$a" "$n" "$r" || true
    case "$REGION_OUTCOME" in
      succeeded)
        region_objs+=("$(jq -nc --arg r "$r" --argjson rc "$REGION_ROW_COUNT" --arg obs "$REGION_OBSERVED_AT" \
          '{region:$r, outcome:"succeeded", row_count:$rc, observed_at:$obs}')")
        ;;
      partial)
        echo "$a,\"$n\",$r,describe,failed" >> "$OUT/failures.csv"
        region_objs+=("$(jq -nc --arg r "$r" --argjson rc "$REGION_ROW_COUNT" --arg obs "$REGION_OBSERVED_AT" \
          '{region:$r, outcome:"partial", stage:"describe", row_count:$rc, observed_at:$obs}')")
        ;;
      failed)
        echo "$a,\"$n\",$r,describe,failed" >> "$OUT/failures.csv"
        region_objs+=("$(jq -nc --arg r "$r" --arg obs "$REGION_OBSERVED_AT" \
          '{region:$r, outcome:"failed", stage:"describe", observed_at:$obs}')")
        ;;
    esac
  done

  local regions_json
  if [ "${#region_objs[@]}" -eq 0 ]; then
    regions_json='[]'
  else
    regions_json=$(printf '%s\n' "${region_objs[@]}" | jq -s 'sort_by(.region)')
  fi
  jq -n --arg a "$a" --arg n "$n" --arg cred "$cred" --arg src "$region_source" --argjson regions "$regions_json" '
    {account_id:$a, account_name:$n, credential_source:$cred, regions_attempted:"known",
     not_attempted_reason: null, region_source:$src, regions:$regions}' \
    >> "$RUN_FRAGMENTS_DIR/accounts.ndjson"
}

aws organizations list-accounts --output json > "$OUT/accounts.json"
jq -r '.Accounts[] | select(.Status=="ACTIVE") | [.Id,.Name] | @tsv' "$OUT/accounts.json" |
while IFS=$'\t' read -r account name; do
  (  # subshell: member-account credentials must not leak into the next assume-role
    attempted_set=known
    [ -z "$REGIONS" ] && attempted_set=unknown
    # If MANAGEMENT_ACCOUNT_ID is set and matches current account, use caller's credentials
    if [ -n "$MANAGEMENT_ACCOUNT_ID" ] && [ "$account" = "$MANAGEMENT_ACCOUNT_ID" ]; then
      scan_account "$account" "$name" "management-account"
    else
      creds=$(aws sts assume-role \
                --role-arn "arn:aws:iam::${account}:role/${ROLE_NAME}" \
                --role-session-name ipam-inventory --duration-seconds 900 --output json) \
        || {
          echo "$account,\"$name\",,assume-role,failed" >> "$OUT/failures.csv"
          record_account_unattempted "$account" "$name" "assumed-role" "assume-role-failed" "$attempted_set"
          exit 0
        }
      # SC2155 (declare and assign separately) is about a command's exit status
      # being masked by `export`. It is masked here on purpose: `creds` was
      # checked above, jq on it cannot fail in a way that matters, and the
      # identity check two lines down refuses the account if the keys are wrong.
      # shellcheck disable=SC2155
      export AWS_ACCESS_KEY_ID=$(jq -r .Credentials.AccessKeyId <<<"$creds")
      # shellcheck disable=SC2155
      export AWS_SECRET_ACCESS_KEY=$(jq -r .Credentials.SecretAccessKey <<<"$creds")
      # shellcheck disable=SC2155
      export AWS_SESSION_TOKEN=$(jq -r .Credentials.SessionToken <<<"$creds")
      seen=$(aws sts get-caller-identity --query Account --output text)
      [ "$seen" = "$account" ] || {
        echo "$account,\"$name\",,identity,got $seen" >> "$OUT/failures.csv"
        # The mismatched account id ($seen) is deliberately not carried into
        # run.json: it can be a real AWS account beyond anything accounts.json
        # already holds, which the collector must never write anywhere.
        record_account_unattempted "$account" "$name" "assumed-role" "identity-mismatch" "$attempted_set"
        exit 0
      }
      scan_account "$account" "$name" "assumed-role"
    fi
  )
done

finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

if [ -n "$REGIONS" ]; then
  # $REGIONS is intentionally left unquoted here: it is a space-separated
  # list (the same splitting scan_account already relies on in its own
  # "for r in $regions" loop), and word-splitting it is what turns it into
  # one line per region for jq to read.
  # shellcheck disable=SC2086
  configured_regions_json=$(printf '%s\n' $REGIONS | jq -R -s 'split("\n") | map(select(length>0))')
else
  configured_regions_json='null'
fi

if [ -n "$MANAGEMENT_ACCOUNT_ID" ]; then
  mgmt_used=true
else
  mgmt_used=false
fi

accounts_json=$(jq -s 'sort_by(.account_id)' "$RUN_FRAGMENTS_DIR/accounts.ndjson")

jq -n \
  --arg script_version "$SCRIPT_VERSION" \
  --arg started "$started_at" \
  --arg finished "$finished_at" \
  --arg role "$ROLE_NAME" \
  --argjson mgmt_used "$mgmt_used" \
  --arg mgmt_id "${MANAGEMENT_ACCOUNT_ID:-}" \
  --argjson configured_regions "$configured_regions_json" \
  --argjson accounts "$accounts_json" \
  '{
     script_version: $script_version,
     started_at: $started,
     finished_at: $finished,
     role_name: $role,
     management_account_used: $mgmt_used,
     management_account_id: (if $mgmt_used then $mgmt_id else null end),
     configured_regions: $configured_regions,
     accounts: $accounts
   }' > "$OUT/run.json"

echo "networks: $(($(wc -l < "$OUT/networks.csv") - 1))   failures: $(($(wc -l < "$OUT/failures.csv") - 1))   run.json: $(jq '.accounts | length' "$OUT/run.json") account(s)"
