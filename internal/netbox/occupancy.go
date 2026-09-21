package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Onboarding import writes occupancy, never allocations: an imported network
// blocks address space and owns nothing (ADR 0007). allocationIDCF,
// allocationKeyCF, operationIDCF and stateCF mark ledger-owned prefixes and
// the end-to-end suite asserts that every prefix carrying an allocation id is
// known to the API -- so nothing here writes them, and an object that already
// carries one is refused rather than modified.
const (
	// ImportedTag must exist in NetBox before an import runs: tag assignment
	// resolves an existing tag by slug and fails the create otherwise.
	ImportedTag = "platform-ipam-imported"
	// ImportBatchField and ImportSourceField let a whole import be listed or
	// removed later, and are the only custom fields written on a range.
	ImportBatchField  = "platform_import_batch"
	ImportSourceField = "platform_import_source"
	// ImportContributorsField holds the JSON array of cloud resources that
	// occupy an imported prefix (ADR 0016). It is UNOWNED: ownedFields never
	// names it, so an adoption's merge preserves it and an abandonment's clear
	// leaves it alone, exactly as the import tag, batch and source already
	// survive both. TestOwnedFieldsDoNotOwnTheContributorList is what keeps
	// that true -- ADR 0012 is the precedent for testing it rather than
	// assuming it, because the AWS account and region were believed to survive
	// an adoption until review measured that they did not.
	//
	// The type is NetBox's own `json`, measured against the pinned image on
	// 2026-09-22 (ADR 0016's dated paragraph): it round-trips as a structured
	// array rather than as a string, and the server refused nothing up to
	// 50 000 entries. It is defined for prefixes only, which is why
	// ensureOccupancyRange refuses contributors outright rather than dropping
	// them silently.
	ImportContributorsField = "platform_import_contributors"

	awsAccountCF  = "platform_aws_account_id"
	awsRegionCF   = "platform_aws_region"
	awsResourceCF = "platform_aws_resource_id"

	// NetBox truncates nothing: a longer description is a 400 whose body the
	// adapter deliberately does not surface, so the limit is checked here.
	maxDescription = 200
)

// Actions reported by EnsureOccupancy.
const (
	OccupancyCreated   = "created"
	OccupancyUnchanged = "unchanged"
)

// Object kinds written by EnsureOccupancy.
const (
	OccupancyPrefix = "prefix"
	OccupancyRange  = "range"
)

// Refusals a caller can classify with errors.Is. Each one is a case where
// writing would either corrupt ledger-owned data or produce occupancy the
// allocator does not read the way the operator expects.
var (
	ErrOccupancyManaged  = errors.New("refusing to import over a platform-managed object")
	ErrOccupancyPool     = errors.New("refusing to import a configured pool's own address space")
	ErrOccupancyConflict = errors.New("conflicting NetBox object")
	ErrOccupancyInvalid  = errors.New("invalid occupancy")
	ErrOccupancyNoVRF    = errors.New("domain has no VRF configured")
)

// Occupancy is one row of an onboarding import: a network given as a CIDR, or
// an address range given as a start and an end address. Batch names the import
// so it can be found again; the AWS attributes are written only for a network,
// because a range carries no account in the canonical table.
type Occupancy struct {
	CIDR          string
	StartAddress  string
	EndAddress    string
	Description   string
	Batch         string
	Source        string
	AWSAccountID  string
	AWSRegion     string
	AWSResourceID string
	// Contributors are the cloud resources this row's collapsed group says
	// occupy the CIDR (ADR 0016). They are written on CREATE only: there is
	// no update path for occupancy in this package -- ADR 0012 enumerated
	// every prefix write it has and ADR 0016 makes the refresh package M9b3's
	// work -- so an existing prefix is still returned untouched, whatever
	// this list says.
	//
	// A range carries none: the canonical ranges table has no account,
	// resource or region column, and the field is defined for prefixes only.
	Contributors []domain.Contributor
}

// OccupancyResult reports what one call did, so an import can summarise a
// batch without reading NetBox again.
type OccupancyResult struct {
	Kind   string // OccupancyPrefix or OccupancyRange
	ID     string // NetBox object ID
	Action string // OccupancyCreated or OccupancyUnchanged
}

// EnsureOccupancy creates, or confirms, one unmanaged object in the domain's
// VRF. It is the single write path for onboarding import, so the VRF, marker
// and pool rules live beside Ensure rather than in the command. A second
// identical call writes nothing and reports OccupancyUnchanged.
func (c *Client) EnsureOccupancy(ctx context.Context, d domain.Domain, o Occupancy) (OccupancyResult, error) {
	cfg, pools, err := c.occupancyDomain(d)
	if err != nil {
		return OccupancyResult{}, err
	}
	if o.Batch == "" {
		return OccupancyResult{}, fmt.Errorf("%w: a batch name is required", ErrOccupancyInvalid)
	}
	if len([]rune(o.Description)) > maxDescription {
		return OccupancyResult{}, fmt.Errorf("%w: description exceeds NetBox's %d-character limit", ErrOccupancyInvalid, maxDescription)
	}
	// An entry with no identity is not evidence of anything, and ADR 0016
	// builds every removal argument on the identity string. Refuse here
	// rather than write a list a later package must distrust one entry at a
	// time.
	for i, c := range o.Contributors {
		if c.Identity == "" {
			return OccupancyResult{}, fmt.Errorf("%w: contributor %d has no identity", ErrOccupancyInvalid, i)
		}
	}
	hasRange := o.StartAddress != "" || o.EndAddress != ""
	switch {
	case o.CIDR != "" && hasRange:
		return OccupancyResult{}, fmt.Errorf("%w: a row is either a CIDR or an address range, not both", ErrOccupancyInvalid)
	case o.CIDR != "":
		return c.ensureOccupancyPrefix(ctx, cfg.Backend.VRFID, pools, o)
	case o.StartAddress != "" && o.EndAddress != "":
		return c.ensureOccupancyRange(ctx, cfg.Backend.VRFID, pools, o)
	}
	return OccupancyResult{}, fmt.Errorf("%w: a CIDR, or a start and an end address, is required", ErrOccupancyInvalid)
}

// occupancyDomain resolves the configured domain instead of trusting the
// caller's copy. A prefix written into any other VRF is invisible to Snapshot,
// so it would look imported while blocking nothing.
func (c *Client) occupancyDomain(d domain.Domain) (domain.Domain, []domain.Pool, error) {
	if d.ID == "" {
		return domain.Domain{}, nil, errors.New("domain ID is required")
	}
	cfg, ok := c.domains[d.ID]
	if !ok {
		return domain.Domain{}, nil, fmt.Errorf("unknown domain %q", d.ID)
	}
	if cfg.Backend.VRFID == 0 {
		return domain.Domain{}, nil, fmt.Errorf("%w: %s", ErrOccupancyNoVRF, cfg.ID)
	}
	if d.Backend.VRFID != 0 && d.Backend.VRFID != cfg.Backend.VRFID {
		return domain.Domain{}, nil, fmt.Errorf("domain %s VRF %d differs from the configured VRF %d", cfg.ID, d.Backend.VRFID, cfg.Backend.VRFID)
	}
	pools, err := c.domainPools(cfg)
	if err != nil {
		return domain.Domain{}, nil, err
	}
	return cfg, pools, nil
}

func (c *Client) ensureOccupancyPrefix(ctx context.Context, vrfID int, pools []domain.Pool, o Occupancy) (OccupancyResult, error) {
	cidr, err := occupancyPrefix(o.CIDR)
	if err != nil {
		return OccupancyResult{}, err
	}
	if err := refusePoolPrefix(cidr, pools); err != nil {
		return OccupancyResult{}, err
	}
	existing, err := c.prefixesAt(ctx, cidr, vrfID)
	if err != nil {
		return OccupancyResult{}, err
	}
	if len(existing) > 1 {
		return OccupancyResult{}, fmt.Errorf("%w: %d prefixes already hold %s in VRF %d", ErrOccupancyConflict, len(existing), cidr, vrfID)
	}
	if len(existing) == 1 {
		// Whatever it is, the space is already occupied and the import is
		// complete. Operator-owned fields -- description, tenant, other tags --
		// are left exactly as they are.
		return existingOccupancyPrefix(existing[0])
	}
	fields := map[string]any{ImportBatchField: o.Batch, ImportSourceField: o.Source}
	for key, value := range map[string]string{awsAccountCF: o.AWSAccountID, awsRegionCF: o.AWSRegion, awsResourceCF: o.AWSResourceID} {
		if value != "" {
			fields[key] = value
		}
	}
	// Only on the create path, and only when there is something to say. An
	// empty key would write an empty JSON array, which ADR 0016 defines as
	// "a list nobody wrote" and refuses a removal on -- the opposite of the
	// absent field a pre-ADR-0016 prefix carries, and not what an import with
	// no rows to record means.
	if len(o.Contributors) > 0 {
		fields[ImportContributorsField] = o.Contributors
	}
	if err := refuseManagedFields(fields); err != nil {
		return OccupancyResult{}, err
	}
	payload := map[string]any{"prefix": cidr.String(), "vrf": vrfID, "status": "active", "description": o.Description,
		"tags": []any{map[string]any{"slug": ImportedTag}}, "custom_fields": fields}
	x, err := createOccupancy[prefix](ctx, c, "/api/ipam/prefixes/", payload)
	if err != nil {
		return c.recoverOccupancyPrefix(ctx, cidr, vrfID, err)
	}
	got, err := parsePrefix(x.Prefix)
	if err != nil || got != cidr || x.vrfID() != vrfID {
		return OccupancyResult{}, fmt.Errorf("NetBox created %q in VRF %d, expected %s in VRF %d", x.Prefix, x.vrfID(), cidr, vrfID)
	}
	if _, err := existingOccupancyPrefix(x); err != nil {
		return OccupancyResult{}, err
	}
	return OccupancyResult{Kind: OccupancyPrefix, ID: strconv.Itoa(x.ID), Action: OccupancyCreated}, nil
}

func (c *Client) ensureOccupancyRange(ctx context.Context, vrfID int, pools []domain.Pool, o Occupancy) (OccupancyResult, error) {
	start, err := occupancyAddr(o.StartAddress)
	if err != nil {
		return OccupancyResult{}, err
	}
	end, err := occupancyAddr(o.EndAddress)
	if err != nil {
		return OccupancyResult{}, err
	}
	if start.Compare(end) > 0 {
		return OccupancyResult{}, fmt.Errorf("%w: range %s-%s ends before it starts", ErrOccupancyInvalid, start, end)
	}
	// The AWS custom fields are defined for prefixes only, and the canonical
	// ranges table has no account column. Refusing keeps a wiring mistake from
	// dropping the attributes silently.
	if o.AWSAccountID != "" || o.AWSRegion != "" || o.AWSResourceID != "" {
		return OccupancyResult{}, fmt.Errorf("%w: AWS attributes are not written on an imported range", ErrOccupancyInvalid)
	}
	// Same reasoning for the contributor list, and the same refusal rather
	// than a silent drop: platform_import_contributors is defined for
	// ipam.prefix only (deploy/compose/seed-netbox.py), so sending it here
	// would be a 400 whose body this adapter deliberately does not surface.
	if len(o.Contributors) > 0 {
		return OccupancyResult{}, fmt.Errorf("%w: contributors are not written on an imported range", ErrOccupancyInvalid)
	}
	if err := refusePoolSpan(start, end, pools); err != nil {
		return OccupancyResult{}, err
	}
	existing, err := c.rangesIn(ctx, vrfID)
	if err != nil {
		return OccupancyResult{}, err
	}
	if result, found, err := matchOccupancyRange(existing, start, end); found || err != nil {
		return result, err
	}
	fields := map[string]any{ImportBatchField: o.Batch, ImportSourceField: o.Source}
	if err := refuseManagedFields(fields); err != nil {
		return OccupancyResult{}, err
	}
	payload := map[string]any{"start_address": start.String(), "end_address": end.String(), "vrf": vrfID,
		"status": "active", "description": o.Description,
		"tags": []any{map[string]any{"slug": ImportedTag}}, "custom_fields": fields}
	r, err := createOccupancy[occupancyRange](ctx, c, "/api/ipam/ip-ranges/", payload)
	if err != nil {
		return c.recoverOccupancyRange(ctx, vrfID, start, end, err)
	}
	gotStart, errStart := hostAddr(r.StartAddress)
	gotEnd, errEnd := hostAddr(r.EndAddress)
	if errStart != nil || errEnd != nil || gotStart != start || gotEnd != end || r.vrfID() != vrfID {
		return OccupancyResult{}, fmt.Errorf("NetBox created %q-%q in VRF %d, expected %s-%s in VRF %d", r.StartAddress, r.EndAddress, r.vrfID(), start, end, vrfID)
	}
	if r.ID == 0 {
		return OccupancyResult{}, errors.New("NetBox create returned no ID")
	}
	return OccupancyResult{Kind: OccupancyRange, ID: strconv.Itoa(r.ID), Action: OccupancyCreated}, nil
}

// occupancyRange decodes the fields an import compares. The ipRange type
// Snapshot uses carries no custom fields, and gains none here.
type occupancyRange struct {
	ID           int            `json:"id"`
	StartAddress string         `json:"start_address"`
	EndAddress   string         `json:"end_address"`
	VRF          any            `json:"vrf"`
	CustomFields map[string]any `json:"custom_fields"`
}

func (r occupancyRange) vrfID() int { return intID(r.VRF) }

// createOccupancy posts one object and decodes NetBox's echo of it. The caller
// checks identity: a create whose response says something else must not be
// reported as the requested write.
func createOccupancy[T any](ctx context.Context, c *Client, path string, payload map[string]any) (T, error) {
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

// existingOccupancyPrefix decides what a prefix that already holds the CIDR
// means for an import.
func existingOccupancyPrefix(x prefix) (OccupancyResult, error) {
	if id := stringCF(x.CustomFields, allocationIDCF); id != "" {
		return OccupancyResult{}, fmt.Errorf("%w: prefix %d carries %s %q", ErrOccupancyManaged, x.ID, allocationIDCF, id)
	}
	if x.ID == 0 {
		return OccupancyResult{}, errors.New("NetBox returned a prefix without an ID")
	}
	return OccupancyResult{Kind: OccupancyPrefix, ID: strconv.Itoa(x.ID), Action: OccupancyUnchanged}, nil
}

// matchOccupancyRange looks for the range an import would create. NetBox
// refuses overlapping ranges in a VRF, so an overlapping but unequal one is
// reported here rather than as an opaque 400.
func matchOccupancyRange(existing []occupancyRange, start, end netip.Addr) (OccupancyResult, bool, error) {
	for _, r := range existing {
		rs, errStart := hostAddr(r.StartAddress)
		re, errEnd := hostAddr(r.EndAddress)
		if errStart != nil || errEnd != nil {
			return OccupancyResult{}, true, fmt.Errorf("IP range %d has unreadable addresses %q-%q", r.ID, r.StartAddress, r.EndAddress)
		}
		if rs == start && re == end {
			if id := stringCF(r.CustomFields, allocationIDCF); id != "" {
				return OccupancyResult{}, true, fmt.Errorf("%w: IP range %d carries %s %q", ErrOccupancyManaged, r.ID, allocationIDCF, id)
			}
			if r.ID == 0 {
				return OccupancyResult{}, true, errors.New("NetBox returned an IP range without an ID")
			}
			return OccupancyResult{Kind: OccupancyRange, ID: strconv.Itoa(r.ID), Action: OccupancyUnchanged}, true, nil
		}
		if rs.Compare(end) <= 0 && re.Compare(start) >= 0 {
			return OccupancyResult{}, true, fmt.Errorf("%w: range %s-%s overlaps IP range %d (%s-%s)", ErrOccupancyConflict, start, end, r.ID, rs, re)
		}
	}
	return OccupancyResult{}, false, nil
}

// refuseManagedFields is a guard for later edits: no import may set a field
// that makes an object look ledger-owned.
func refuseManagedFields(fields map[string]any) error {
	for _, key := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		if _, present := fields[key]; present {
			return fmt.Errorf("%w: an import must not set %s", ErrOccupancyManaged, key)
		}
	}
	return nil
}

// occupancyPrefix parses an imported CIDR. A non-canonical value is refused
// rather than masked, because the row may be a host address typed into a
// network column; v1 allocates IPv4 only, and an IPv6 row would occupy space
// no pool serves.
func occupancyPrefix(s string) (netip.Prefix, error) {
	p, err := parsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: %v", ErrOccupancyInvalid, err)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("%w: %s is not a canonical network address", ErrOccupancyInvalid, strings.TrimSpace(s))
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%w: %s is not IPv4", ErrOccupancyInvalid, strings.TrimSpace(s))
	}
	return p, nil
}

func hostAddr(s string) (netip.Addr, error) {
	return netip.ParseAddr(strings.Split(strings.TrimSpace(s), "/")[0])
}

func occupancyAddr(s string) (netip.Addr, error) {
	a, err := hostAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %v", ErrOccupancyInvalid, err)
	}
	if !a.Is4() {
		return netip.Addr{}, fmt.Errorf("%w: %s is not IPv4", ErrOccupancyInvalid, strings.TrimSpace(s))
	}
	return a, nil
}

// refusePoolPrefix keeps an import away from a pool's own space. Snapshot
// requires exactly one prefix per configured pool -- a second one fails the
// whole snapshot, and every reservation in the domain then returns 503 -- and
// it drops a prefix that merely contains a pool as an ancestor, so such a row
// would block nothing while appearing to be imported.
func refusePoolPrefix(cidr netip.Prefix, pools []domain.Pool) error {
	for _, p := range pools {
		pc, err := parsePrefix(p.CIDR)
		if err != nil {
			return fmt.Errorf("pool %s CIDR: %w", p.ID, err)
		}
		pc = pc.Masked()
		switch {
		case pc == cidr:
			return fmt.Errorf("%w: %s is pool %s", ErrOccupancyPool, cidr, p.ID)
		case cidr.Bits() < pc.Bits() && cidr.Contains(pc.Addr()):
			return fmt.Errorf("%w: %s contains pool %s (%s), which the inventory snapshot ignores as an ancestor", ErrOccupancyPool, cidr, p.ID, pc)
		}
	}
	return nil
}

// refusePoolSpan is the range equivalent. Snapshot turns a range into covering
// blocks without the ancestor rule that protects prefixes, so a range spanning
// a pool marks the entire pool occupied and takes the domain's allocation
// offline.
func refusePoolSpan(start, end netip.Addr, pools []domain.Pool) error {
	for _, p := range pools {
		pc, err := parsePrefix(p.CIDR)
		if err != nil {
			return fmt.Errorf("pool %s CIDR: %w", p.ID, err)
		}
		pc = pc.Masked()
		if start.Compare(pc.Addr()) <= 0 && end.Compare(prefixLast(pc)) >= 0 {
			return fmt.Errorf("%w: range %s-%s spans pool %s (%s)", ErrOccupancyPool, start, end, p.ID, pc)
		}
	}
	return nil
}

// prefixesAt returns the prefixes NetBox holds for exactly this CIDR in this
// VRF. The filter narrows the read; the value and the VRF are compared again
// locally, as Ensure does, so a filter a NetBox release changes cannot widen
// what an import treats as the same network.
func (c *Client) prefixesAt(ctx context.Context, cidr netip.Prefix, vrfID int) ([]prefix, error) {
	var found []prefix
	if err := c.page(ctx, "/api/ipam/prefixes/?prefix="+url.QueryEscape(cidr.String()), func(raw json.RawMessage) error {
		var x prefix
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		got, err := parsePrefix(x.Prefix)
		if err != nil {
			return fmt.Errorf("prefix %d: %w", x.ID, err)
		}
		if got == cidr && x.vrfID() == vrfID {
			found = append(found, x)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// rangesIn lists the VRF's IP ranges. NetBox matches start_address against the
// stored inet value, whose mask an import does not control, so the comparison
// is done locally on host addresses instead.
func (c *Client) rangesIn(ctx context.Context, vrfID int) ([]occupancyRange, error) {
	var found []occupancyRange
	if err := c.page(ctx, "/api/ipam/ip-ranges/?vrf_id="+strconv.Itoa(vrfID), func(raw json.RawMessage) error {
		var r occupancyRange
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.vrfID() == vrfID {
			found = append(found, r)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

// recoverOccupancyPrefix resolves an uncertain create. Occupancy owns nothing,
// so the only question is whether the space is now blocked; if the CIDR is
// present and unmanaged the import is complete, whoever wrote it.
func (c *Client) recoverOccupancyPrefix(ctx context.Context, cidr netip.Prefix, vrfID int, original error) (OccupancyResult, error) {
	found, err := c.prefixesAt(ctx, cidr, vrfID)
	if err != nil || len(found) != 1 {
		return OccupancyResult{}, fmt.Errorf("uncertain NetBox create for %s: %w", cidr, original)
	}
	return existingOccupancyPrefix(found[0])
}

func (c *Client) recoverOccupancyRange(ctx context.Context, vrfID int, start, end netip.Addr, original error) (OccupancyResult, error) {
	existing, err := c.rangesIn(ctx, vrfID)
	if err != nil {
		return OccupancyResult{}, fmt.Errorf("uncertain NetBox create for %s-%s: %w", start, end, original)
	}
	result, found, err := matchOccupancyRange(existing, start, end)
	if !found {
		return OccupancyResult{}, fmt.Errorf("uncertain NetBox create for %s-%s: %w", start, end, original)
	}
	return result, err
}
