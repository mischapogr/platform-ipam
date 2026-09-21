// Onboarding import into netbox-aws-vpc-plugin (package N3, docs/WORK_PLAN.md
// Track N; the plugin evaluation and version pin are in
// docs/NETBOX_AWS_PLUGIN.md; the invariant this file must never break is
// ADR 0009: the plugin is a view, never the allocator's source of truth.
// EnsureOccupancy in occupancy.go writes every imported network as a prefix
// in the domain's VRF; the methods here create or confirm AWS Account, VPC
// and Subnet objects in the plugin that point at those prefixes, and never
// create or modify a prefix themselves. Callers that never set
// `onboard apply --aws-objects` never call anything in this file, so an
// installation without the plugin is byte-for-byte unaffected.
//
// Every Ensure* method here follows occupancy.go's own convention: look the
// object up by the plugin's unique key, create it if absent, and if present
// update only the fields this import owns -- never delete a relationship or
// tag an operator or an earlier run added. A zero value for an optional
// field (an empty Name, a zero RegionNetBoxID) means "not provided, leave
// whatever is there alone", exactly like o.Description being left alone by
// EnsureOccupancy for an object that already exists.
//
// Fields verified against the plugin's real serializers, models and
// filtersets (github.com/dmaclaury/netbox-aws-vpc-plugin 0.1.0,
// netbox_aws_vpc_plugin/{models,api/serializers,filtersets}.py): AWSAccount,
// AWSVPC and AWSSubnet all embed NetBox's NetBoxModelSerializer, so they
// accept "tags" the same way occupancy.go's prefixes and ranges do,
// including by slug. vpc_cidr, subnet_cidr, owner_account, vpc and region are
// declared as nested serializers (PrefixSerializer, NestedAWSAccountSerializer,
// NestedAWSVPCSerializer, RegionSerializer, all through NetBox's
// WritableNestedSerializer base) -- NetBox's own write convention for such a
// field accepts a plain integer primary key, which is what every payload
// below sends. AWSVPCSerializer.owner_account and AWSSubnetSerializer.vpc /
// owner_account are declared with neither `required=False` nor
// `allow_null=True`, so the plugin refuses to create either object without
// them; EnsureAWSVPC and EnsureAWSSubnet refuse locally first, with a clearer
// message than the plugin's 400 body (which this package does not surface,
// matching client.go's HTTPError). AWSSubnet has no availability-zone field
// at all (models/aws_subnet.py carries only a "# TODO: Availability Zone"
// comment in 0.1.0): AWSSubnetSpec.AvailabilityZone is accepted so a caller
// can pass the import table's az_id column through without a wiring error,
// but it is never written anywhere -- see docs/NETBOX_AWS_PLUGIN.md's
// package N3 section for what that means for an operator.
package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Plugin and core NetBox paths this file reads and writes. The accounts/
// vpcs/subnets paths were confirmed against a running plugin, not just its
// source, during package N2 (docs/NETBOX_AWS_PLUGIN.md, "Installed and
// verified"): base_url "aws-vpc" puts the plugin's API root at
// /api/plugins/aws-vpc/, and the router registers aws-accounts, aws-vpcs and
// aws-subnets under it (netbox_aws_vpc_plugin/api/urls.py).
const (
	awsPluginAccountsPath = "/api/plugins/aws-vpc/aws-accounts/"
	awsPluginVPCsPath     = "/api/plugins/aws-vpc/aws-vpcs/"
	awsPluginSubnetsPath  = "/api/plugins/aws-vpc/aws-subnets/"
	dcimRegionsPath       = "/api/dcim/regions/"
)

// Sentinels a caller can classify with errors.Is, in the style of
// occupancy.go's ErrOccupancy* family.
//
// "Plugin not installed" has exactly one sentinel for the whole package:
// ErrAWSPluginAccountsUnavailable, declared in awsplugin_accounts.go
// (package E2, docs/WORK_PLAN.md) -- ProbeAWSPlugin below reuses it rather
// than declaring its own, so onboardcmd's `apply --aws-objects` probe and
// `drift`'s plugin read report the exact same condition the exact same way.
var (
	// ErrAWSObjectInvalid marks a request this file refuses to send: a
	// missing unique key, a missing required relationship (owner account, a
	// subnet's VPC), or a CIDR that does not exist as a NetBox prefix in the
	// domain's VRF yet.
	ErrAWSObjectInvalid = errors.New("invalid AWS plugin object")
	// ErrAWSObjectConflict marks more than one plugin (or dcim.Region)
	// object already carrying the same unique key -- defensive: the
	// plugin's own uniqueness constraints should make this unreachable.
	ErrAWSObjectConflict = errors.New("conflicting AWS plugin object")
)

// Actions reported by every Ensure* method here, mirroring occupancy.go's
// OccupancyCreated/OccupancyUnchanged plus the one case occupancy.go never
// needs: an existing plugin object whose owned fields differ from what this
// import computed.
const (
	AWSObjectCreated   = "created"
	AWSObjectUpdated   = "updated"
	AWSObjectUnchanged = "unchanged"
)

// AWSObjectResult reports what one Ensure/Lookup call did.
type AWSObjectResult struct {
	ID     string // the plugin (or dcim.Region) object's NetBox ID
	Action string // AWSObjectCreated, AWSObjectUpdated or AWSObjectUnchanged
}

// AWSAccountSpec is EnsureAWSAccount's input. Name == "" leaves an existing
// account's name exactly as it is, so a networks-table row -- which carries
// no account name -- never blanks out a name an accounts-table row (or an
// operator) already set.
type AWSAccountSpec struct {
	AccountID string
	Name      string
}

// AWSVPCSpec is EnsureAWSVPC's input. PrimaryCIDR and every entry of
// SecondaryCIDRs must already exist as a NetBox prefix in d's VRF -- this
// file never creates one (ADR 0009) -- and EnsureAWSVPC returns an error
// naming the CIDR if one does not. RegionNetBoxID == 0 leaves an existing
// region untouched; OwnerAccountNetBoxID == 0 is refused outright, because
// AWSVPCSerializer declares owner_account required.
type AWSVPCSpec struct {
	VPCID                string
	Name                 string
	OwnerAccountNetBoxID int
	RegionNetBoxID       int
	PrimaryCIDR          string
	SecondaryCIDRs       []string
}

// AWSSubnetSpec is EnsureAWSSubnet's input. See AWSVPCSpec for the CIDR and
// zero-value conventions; VPCNetBoxID and OwnerAccountNetBoxID are both
// required for the same reason (AWSSubnetSerializer declares vpc and
// owner_account required). AvailabilityZone is accepted but never written --
// see this file's package comment.
type AWSSubnetSpec struct {
	SubnetID             string
	Name                 string
	VPCNetBoxID          int
	OwnerAccountNetBoxID int
	RegionNetBoxID       int
	CIDR                 string
	AvailabilityZone     string
}

// --- decode shapes for the plugin's and dcim's nested-serializer responses ---

// awsRef decodes any of this plugin's single-object nested fields
// (vpc_cidr, subnet_cidr, owner_account, vpc, region): NetBox always renders
// them as an object carrying at least "id" on read, whatever was sent on
// write (a bare integer, per this file's package comment).
type awsRef struct {
	ID int `json:"id"`
}

func refID(r *awsRef) int {
	if r == nil {
		return 0
	}
	return r.ID
}

// awsTag decodes one entry of a NetBoxModelSerializer's "tags" field.
type awsTag struct {
	ID   int    `json:"id"`
	Slug string `json:"slug"`
}

func hasTagSlug(tags []awsTag, slug string) bool {
	for _, t := range tags {
		if t.Slug == slug {
			return true
		}
	}
	return false
}

// tagsWithImport returns the payload for a "tags" field that keeps every
// slug already present and adds ImportedTag -- an addition, never a
// replacement, so an operator's own tags on the object survive.
func tagsWithImport(tags []awsTag) []any {
	out := make([]any, 0, len(tags)+1)
	seen := map[string]bool{}
	for _, t := range tags {
		if t.Slug == "" || seen[t.Slug] {
			continue
		}
		seen[t.Slug] = true
		out = append(out, map[string]any{"slug": t.Slug})
	}
	if !seen[ImportedTag] {
		out = append(out, map[string]any{"slug": ImportedTag})
	}
	return out
}

type awsAccountObj struct {
	ID        int      `json:"id"`
	AccountID string   `json:"account_id"`
	Name      string   `json:"name"`
	Tags      []awsTag `json:"tags"`
}

type awsVPCObj struct {
	ID                    int      `json:"id"`
	VPCID                 string   `json:"vpc_id"`
	Name                  string   `json:"name"`
	VPCCIDR               *awsRef  `json:"vpc_cidr"`
	VPCSecondaryIPv4CIDRs []awsRef `json:"vpc_secondary_ipv4_cidrs"`
	OwnerAccount          *awsRef  `json:"owner_account"`
	Region                *awsRef  `json:"region"`
	Tags                  []awsTag `json:"tags"`
}

type awsSubnetObj struct {
	ID           int      `json:"id"`
	SubnetID     string   `json:"subnet_id"`
	Name         string   `json:"name"`
	SubnetCIDR   *awsRef  `json:"subnet_cidr"`
	VPC          *awsRef  `json:"vpc"`
	OwnerAccount *awsRef  `json:"owner_account"`
	Region       *awsRef  `json:"region"`
	Tags         []awsTag `json:"tags"`
}

type dcimRegionObj struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// --- probe ---

// ProbeAWSPlugin confirms netbox-aws-vpc-plugin is installed and reachable
// with this client's token. onboardcmd's apply --aws-objects calls this
// before any occupancy write (docs/WORK_PLAN.md package N3): a 404 from the
// plugin's own accounts endpoint means the plugin is absent, not merely that
// this particular account is missing. Returns ErrAWSPluginAccountsUnavailable
// (awsplugin_accounts.go, package E2) -- the one sentinel this package uses
// for "the plugin is not installed", shared with ListAWSAccounts/drift.
func (c *Client) ProbeAWSPlugin(ctx context.Context) error {
	resp, err := c.request(ctx, http.MethodGet, awsPluginAccountsPath+"?limit=1", nil)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return fmt.Errorf("%w: GET %s returned 404", ErrAWSPluginAccountsUnavailable, awsPluginAccountsPath)
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// --- shared read/write helpers ---

// createAWSObject posts one plugin (or dcim) object and decodes NetBox's
// echo of it, in the style of occupancy.go's createOccupancy.
func createAWSObject[T any](ctx context.Context, c *Client, path string, payload map[string]any) (T, error) {
	var out T
	resp, err := c.request(ctx, http.MethodPost, path, payload)
	if err != nil {
		return out, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode netbox create response: %w", err)
	}
	return out, nil
}

// patchAWSObject sends a partial update to one existing object. Only the
// keys present in payload are touched -- DRF's PATCH is a partial update by
// default -- so a caller that put only the fields it decided differ never
// risks clearing an unrelated one.
func (c *Client) patchAWSObject(ctx context.Context, path string, id int, payload map[string]any) error {
	resp, err := c.request(ctx, http.MethodPatch, path+strconv.Itoa(id)+"/", payload)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) findAWSAccounts(ctx context.Context, accountID string) ([]awsAccountObj, error) {
	var found []awsAccountObj
	if err := c.page(ctx, awsPluginAccountsPath+"?account_id="+url.QueryEscape(accountID), func(raw json.RawMessage) error {
		var a awsAccountObj
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if a.AccountID == accountID {
			found = append(found, a)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func (c *Client) findAWSVPCs(ctx context.Context, vpcID string) ([]awsVPCObj, error) {
	var found []awsVPCObj
	if err := c.page(ctx, awsPluginVPCsPath+"?vpc_id="+url.QueryEscape(vpcID), func(raw json.RawMessage) error {
		var v awsVPCObj
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if v.VPCID == vpcID {
			found = append(found, v)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func (c *Client) findAWSSubnets(ctx context.Context, subnetID string) ([]awsSubnetObj, error) {
	var found []awsSubnetObj
	if err := c.page(ctx, awsPluginSubnetsPath+"?subnet_id="+url.QueryEscape(subnetID), func(raw json.RawMessage) error {
		var sub awsSubnetObj
		if err := json.Unmarshal(raw, &sub); err != nil {
			return err
		}
		if sub.SubnetID == subnetID {
			found = append(found, sub)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func (c *Client) findDCIMRegion(ctx context.Context, slug string) ([]dcimRegionObj, error) {
	var found []dcimRegionObj
	if err := c.page(ctx, dcimRegionsPath+"?slug="+url.QueryEscape(slug), func(raw json.RawMessage) error {
		var r dcimRegionObj
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.Slug == slug {
			found = append(found, r)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// resolveOccupancyPrefixID looks a CIDR up as an exact NetBox prefix in vrfID
// -- reusing occupancy.go's own canonical-CIDR parsing and exact-match
// lookup -- and refuses rather than creating one: "the prefix must already
// exist in the domain VRF" (docs/WORK_PLAN.md package N3). The prefix is
// normally the one EnsureOccupancy just wrote (or already found unmanaged)
// for this same row, one step earlier in the same apply run.
func (c *Client) resolveOccupancyPrefixID(ctx context.Context, cidrText string, vrfID int) (int, error) {
	cidr, err := occupancyPrefix(cidrText)
	if err != nil {
		return 0, err
	}
	found, err := c.prefixesAt(ctx, cidr, vrfID)
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, fmt.Errorf("%w: prefix %s does not exist in VRF %d; onboard apply must import it as occupancy before --aws-objects can link it", ErrAWSObjectInvalid, cidr, vrfID)
	}
	if len(found) > 1 {
		return 0, fmt.Errorf("%w: %d prefixes already hold %s in VRF %d", ErrAWSObjectConflict, len(found), cidr, vrfID)
	}
	return found[0].ID, nil
}

func intsToAny(ids []int) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// unionSecondary returns the sorted union of an existing M2M relationship
// and the CIDRs this import wants attached, or nil if every wanted ID is
// already present -- so EnsureAWSVPC never sends a "vpc_secondary_ipv4_cidrs"
// PATCH that would drop a secondary CIDR an earlier run or an operator added.
func unionSecondary(existing []awsRef, want []int) []int {
	have := map[int]bool{}
	for _, r := range existing {
		have[r.ID] = true
	}
	added := false
	for _, id := range want {
		if !have[id] {
			have[id] = true
			added = true
		}
	}
	if !added {
		return nil
	}
	out := make([]int, 0, len(have))
	for id := range have {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// --- EnsureAWSAccount ---

// EnsureAWSAccount looks an AWS account up by its unique account_id, creates
// it if absent, and otherwise updates only name (when spec.Name is given)
// and adds the import tag if it is missing. tenant is never set (ADR 0009,
// docs/NETBOX_AWS_PLUGIN.md's "Reviewer verification": accounts are
// configuration, and AWSAccount.tenant is left to an operator).
func (c *Client) EnsureAWSAccount(ctx context.Context, spec AWSAccountSpec) (AWSObjectResult, error) {
	if spec.AccountID == "" {
		return AWSObjectResult{}, fmt.Errorf("%w: account_id is required", ErrAWSObjectInvalid)
	}
	existing, err := c.findAWSAccounts(ctx, spec.AccountID)
	if err != nil {
		return AWSObjectResult{}, err
	}
	if len(existing) > 1 {
		return AWSObjectResult{}, fmt.Errorf("%w: %d AWS accounts already carry account_id %s", ErrAWSObjectConflict, len(existing), spec.AccountID)
	}
	if len(existing) == 0 {
		payload := map[string]any{"account_id": spec.AccountID, "tags": []any{map[string]any{"slug": ImportedTag}}}
		if spec.Name != "" {
			payload["name"] = spec.Name
		}
		out, err := createAWSObject[awsAccountObj](ctx, c, awsPluginAccountsPath, payload)
		if err != nil {
			return AWSObjectResult{}, err
		}
		if out.ID == 0 {
			return AWSObjectResult{}, errors.New("NetBox created an AWS account without an ID")
		}
		return AWSObjectResult{ID: strconv.Itoa(out.ID), Action: AWSObjectCreated}, nil
	}

	cur := existing[0]
	patch := map[string]any{}
	if spec.Name != "" && spec.Name != cur.Name {
		patch["name"] = spec.Name
	}
	if !hasTagSlug(cur.Tags, ImportedTag) {
		patch["tags"] = tagsWithImport(cur.Tags)
	}
	if len(patch) == 0 {
		return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUnchanged}, nil
	}
	if err := c.patchAWSObject(ctx, awsPluginAccountsPath, cur.ID, patch); err != nil {
		return AWSObjectResult{}, err
	}
	return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUpdated}, nil
}

// --- EnsureAWSRegion ---

// EnsureAWSRegion looks a dcim.Region up by slug, creating it if absent. AWS
// region names (e.g. "eu-central-1") are already valid NetBox slugs
// (lowercase letters, digits and hyphens), so the region's name and slug are
// both set to awsRegionName. There is no owned field to update on an
// existing region: the AWS region name is exactly the lookup key.
func (c *Client) EnsureAWSRegion(ctx context.Context, awsRegionName string) (AWSObjectResult, error) {
	if awsRegionName == "" {
		return AWSObjectResult{}, fmt.Errorf("%w: an AWS region name is required", ErrAWSObjectInvalid)
	}
	found, err := c.findDCIMRegion(ctx, awsRegionName)
	if err != nil {
		return AWSObjectResult{}, err
	}
	if len(found) > 1 {
		return AWSObjectResult{}, fmt.Errorf("%w: %d dcim regions already carry slug %s", ErrAWSObjectConflict, len(found), awsRegionName)
	}
	if len(found) == 1 {
		return AWSObjectResult{ID: strconv.Itoa(found[0].ID), Action: AWSObjectUnchanged}, nil
	}
	out, err := createAWSObject[dcimRegionObj](ctx, c, dcimRegionsPath, map[string]any{"name": awsRegionName, "slug": awsRegionName})
	if err != nil {
		return AWSObjectResult{}, err
	}
	if out.ID == 0 {
		return AWSObjectResult{}, errors.New("NetBox created a dcim region without an ID")
	}
	return AWSObjectResult{ID: strconv.Itoa(out.ID), Action: AWSObjectCreated}, nil
}

// --- EnsureAWSVPC ---

// EnsureAWSVPC looks an AWS VPC up by its unique vpc_id, creates it if
// absent, and otherwise updates only the fields this import owns (name,
// owner_account, region, vpc_cidr, and adding to -- never removing from --
// vpc_secondary_ipv4_cidrs and tags). vpc_cidr is a plain ForeignKey, not
// one-to-one (docs/NETBOX_AWS_PLUGIN.md's "Reviewer verification"), so
// calling this twice with the same PrimaryCIDR but a different VPCID and
// OwnerAccountNetBoxID deliberately creates two AWSVPC objects pointing at
// the same prefix -- the point of ADR 0009.
func (c *Client) EnsureAWSVPC(ctx context.Context, d domain.Domain, spec AWSVPCSpec) (AWSObjectResult, error) {
	if spec.VPCID == "" {
		return AWSObjectResult{}, fmt.Errorf("%w: vpc_id is required", ErrAWSObjectInvalid)
	}
	if spec.OwnerAccountNetBoxID == 0 {
		return AWSObjectResult{}, fmt.Errorf("%w: vpc %s has no owner account", ErrAWSObjectInvalid, spec.VPCID)
	}
	cfg, _, err := c.occupancyDomain(d)
	if err != nil {
		return AWSObjectResult{}, err
	}
	vrfID := cfg.Backend.VRFID

	var primaryID int
	if spec.PrimaryCIDR != "" {
		primaryID, err = c.resolveOccupancyPrefixID(ctx, spec.PrimaryCIDR, vrfID)
		if err != nil {
			return AWSObjectResult{}, err
		}
	}
	secondaryIDs := make([]int, 0, len(spec.SecondaryCIDRs))
	for _, cidrText := range spec.SecondaryCIDRs {
		id, err := c.resolveOccupancyPrefixID(ctx, cidrText, vrfID)
		if err != nil {
			return AWSObjectResult{}, err
		}
		secondaryIDs = append(secondaryIDs, id)
	}

	existing, err := c.findAWSVPCs(ctx, spec.VPCID)
	if err != nil {
		return AWSObjectResult{}, err
	}
	if len(existing) > 1 {
		return AWSObjectResult{}, fmt.Errorf("%w: %d AWS VPCs already carry vpc_id %s", ErrAWSObjectConflict, len(existing), spec.VPCID)
	}
	if len(existing) == 0 {
		payload := map[string]any{"vpc_id": spec.VPCID, "owner_account": spec.OwnerAccountNetBoxID,
			"tags": []any{map[string]any{"slug": ImportedTag}}}
		if spec.Name != "" {
			payload["name"] = spec.Name
		}
		if primaryID != 0 {
			payload["vpc_cidr"] = primaryID
		}
		if spec.RegionNetBoxID != 0 {
			payload["region"] = spec.RegionNetBoxID
		}
		if len(secondaryIDs) > 0 {
			payload["vpc_secondary_ipv4_cidrs"] = intsToAny(secondaryIDs)
		}
		out, err := createAWSObject[awsVPCObj](ctx, c, awsPluginVPCsPath, payload)
		if err != nil {
			return AWSObjectResult{}, err
		}
		if out.ID == 0 {
			return AWSObjectResult{}, errors.New("NetBox created an AWS VPC without an ID")
		}
		return AWSObjectResult{ID: strconv.Itoa(out.ID), Action: AWSObjectCreated}, nil
	}

	cur := existing[0]
	patch := map[string]any{}
	if spec.Name != "" && spec.Name != cur.Name {
		patch["name"] = spec.Name
	}
	if refID(cur.OwnerAccount) != spec.OwnerAccountNetBoxID {
		patch["owner_account"] = spec.OwnerAccountNetBoxID
	}
	if spec.RegionNetBoxID != 0 && refID(cur.Region) != spec.RegionNetBoxID {
		patch["region"] = spec.RegionNetBoxID
	}
	if primaryID != 0 && refID(cur.VPCCIDR) != primaryID {
		patch["vpc_cidr"] = primaryID
	}
	if want := unionSecondary(cur.VPCSecondaryIPv4CIDRs, secondaryIDs); want != nil {
		patch["vpc_secondary_ipv4_cidrs"] = intsToAny(want)
	}
	if !hasTagSlug(cur.Tags, ImportedTag) {
		patch["tags"] = tagsWithImport(cur.Tags)
	}
	if len(patch) == 0 {
		return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUnchanged}, nil
	}
	if err := c.patchAWSObject(ctx, awsPluginVPCsPath, cur.ID, patch); err != nil {
		return AWSObjectResult{}, err
	}
	return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUpdated}, nil
}

// LookupAWSVPC finds an existing AWS VPC by its unique vpc_id without
// creating or modifying anything. onboardcmd uses this to link a subnet row
// to a VPC that already exists in the plugin from an earlier apply run but
// is not itself part of the current table (docs/WORK_PLAN.md package N3: "a
// VPC ... already exists in the plugin").
func (c *Client) LookupAWSVPC(ctx context.Context, vpcID string) (AWSObjectResult, bool, error) {
	found, err := c.findAWSVPCs(ctx, vpcID)
	if err != nil {
		return AWSObjectResult{}, false, err
	}
	if len(found) == 0 {
		return AWSObjectResult{}, false, nil
	}
	if len(found) > 1 {
		return AWSObjectResult{}, false, fmt.Errorf("%w: %d AWS VPCs already carry vpc_id %s", ErrAWSObjectConflict, len(found), vpcID)
	}
	return AWSObjectResult{ID: strconv.Itoa(found[0].ID), Action: AWSObjectUnchanged}, true, nil
}

// --- EnsureAWSSubnet ---

// EnsureAWSSubnet looks an AWS subnet up by its unique subnet_id, creates it
// if absent, and otherwise updates only the fields this import owns. See
// EnsureAWSVPC for the shared conventions; spec.AvailabilityZone is accepted
// but never written (this file's package comment explains why).
func (c *Client) EnsureAWSSubnet(ctx context.Context, d domain.Domain, spec AWSSubnetSpec) (AWSObjectResult, error) {
	if spec.SubnetID == "" {
		return AWSObjectResult{}, fmt.Errorf("%w: subnet_id is required", ErrAWSObjectInvalid)
	}
	if spec.VPCNetBoxID == 0 {
		return AWSObjectResult{}, fmt.Errorf("%w: subnet %s has no parent VPC", ErrAWSObjectInvalid, spec.SubnetID)
	}
	if spec.OwnerAccountNetBoxID == 0 {
		return AWSObjectResult{}, fmt.Errorf("%w: subnet %s has no owner account", ErrAWSObjectInvalid, spec.SubnetID)
	}
	cfg, _, err := c.occupancyDomain(d)
	if err != nil {
		return AWSObjectResult{}, err
	}

	var cidrID int
	if spec.CIDR != "" {
		cidrID, err = c.resolveOccupancyPrefixID(ctx, spec.CIDR, cfg.Backend.VRFID)
		if err != nil {
			return AWSObjectResult{}, err
		}
	}

	existing, err := c.findAWSSubnets(ctx, spec.SubnetID)
	if err != nil {
		return AWSObjectResult{}, err
	}
	if len(existing) > 1 {
		return AWSObjectResult{}, fmt.Errorf("%w: %d AWS subnets already carry subnet_id %s", ErrAWSObjectConflict, len(existing), spec.SubnetID)
	}
	if len(existing) == 0 {
		payload := map[string]any{"subnet_id": spec.SubnetID, "vpc": spec.VPCNetBoxID, "owner_account": spec.OwnerAccountNetBoxID,
			"tags": []any{map[string]any{"slug": ImportedTag}}}
		if spec.Name != "" {
			payload["name"] = spec.Name
		}
		if cidrID != 0 {
			payload["subnet_cidr"] = cidrID
		}
		if spec.RegionNetBoxID != 0 {
			payload["region"] = spec.RegionNetBoxID
		}
		out, err := createAWSObject[awsSubnetObj](ctx, c, awsPluginSubnetsPath, payload)
		if err != nil {
			return AWSObjectResult{}, err
		}
		if out.ID == 0 {
			return AWSObjectResult{}, errors.New("NetBox created an AWS subnet without an ID")
		}
		return AWSObjectResult{ID: strconv.Itoa(out.ID), Action: AWSObjectCreated}, nil
	}

	cur := existing[0]
	patch := map[string]any{}
	if spec.Name != "" && spec.Name != cur.Name {
		patch["name"] = spec.Name
	}
	if refID(cur.VPC) != spec.VPCNetBoxID {
		patch["vpc"] = spec.VPCNetBoxID
	}
	if refID(cur.OwnerAccount) != spec.OwnerAccountNetBoxID {
		patch["owner_account"] = spec.OwnerAccountNetBoxID
	}
	if spec.RegionNetBoxID != 0 && refID(cur.Region) != spec.RegionNetBoxID {
		patch["region"] = spec.RegionNetBoxID
	}
	if cidrID != 0 && refID(cur.SubnetCIDR) != cidrID {
		patch["subnet_cidr"] = cidrID
	}
	if !hasTagSlug(cur.Tags, ImportedTag) {
		patch["tags"] = tagsWithImport(cur.Tags)
	}
	if len(patch) == 0 {
		return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUnchanged}, nil
	}
	if err := c.patchAWSObject(ctx, awsPluginSubnetsPath, cur.ID, patch); err != nil {
		return AWSObjectResult{}, err
	}
	return AWSObjectResult{ID: strconv.Itoa(cur.ID), Action: AWSObjectUpdated}, nil
}
