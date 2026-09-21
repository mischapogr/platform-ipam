package adoptcmd

// The read-only verdict for one record, shared by `plan` and by `apply`'s
// own pre-flight re-run of plan over every record before it writes anything.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

// Verdict is what apply would do with one record, as plan reports it.
type Verdict string

const (
	// VerdictWouldAdopt: no allocation exists under this tenant and key yet;
	// apply would create one (a 201).
	VerdictWouldAdopt Verdict = "would_adopt"
	// VerdictWouldReplay: an allocation already exists and is committed;
	// apply would replay it (a 200), writing nothing new.
	VerdictWouldReplay Verdict = "would_replay"
	// VerdictPending: an allocation exists under this tenant and key but has
	// not committed (a prior apply's write was interrupted, or the record
	// names an inventory object at odds with what was actually converted);
	// apply would report it and stop rather than finish it synchronously.
	VerdictPending Verdict = "pending"
	// VerdictDeferred applies only to a subnet whose parent_allocation_key
	// resolves to another vpc record in this same file rather than an
	// already-committed allocation: nothing has adopted that parent yet, so
	// its real admissibility is unknown until apply actually runs the parent
	// row. This is not a refusal -- apply.go's pre-flight gate does not
	// count it as one -- because bundling a parent and its subnet in one
	// file is a legitimate shape of input, even though ADR 0010 also means
	// their subnet will very likely still refuse (its parent is born
	// RESERVED, never ACTIVE, in the same run that adopts it): a runbook
	// fact, not something plan can determine in advance.
	VerdictDeferred Verdict = "deferred_on_parent"
	// VerdictWaitingForParent applies only to a subnet whose parent is a
	// committed allocation that has no binding yet. The service exempts a
	// subnet's own parent VPC from the overlap rule only on the evidence of
	// the parent's binding, and an adopted VPC is born RESERVED and unbound:
	// it gains a binding only once its owning team has tagged the VPC and the
	// worker has observed it. Until then the service would refuse the subnet
	// as overlapping "another cloud resource", which is true and tells the
	// operator nothing; this verdict says what is actually being waited for.
	// It is not a refusal, and apply does not attempt such a record.
	VerdictWaitingForParent Verdict = "waiting_for_parent"
	// VerdictRefused: apply would refuse this record outright.
	VerdictRefused Verdict = "refused"
)

// RecordResult is plan's report for one record.
type RecordResult struct {
	SourceRow     int     `json:"source_row"`
	TenantID      string  `json:"tenant_id"`
	AllocationKey string  `json:"allocation_key"`
	Scope         string  `json:"scope"`
	Verdict       Verdict `json:"verdict"`
	CIDR          string  `json:"cidr,omitempty"`
	PrefixLength  int     `json:"prefix_length,omitempty"`
	PoolID        string  `json:"pool_id,omitempty"`
	DomainID      string  `json:"domain_id,omitempty"`
	AllocationID  string  `json:"allocation_id,omitempty"`
	RefusalCode   string  `json:"refusal_code,omitempty"`
	Message       string  `json:"message,omitempty"`
}

// Report is the JSON document plan (and apply's pre-flight gate) prints to
// stdout: one document, not JSON-Lines, since the whole table is evaluated
// before anything is reported.
type Report struct {
	Records []RecordResult `json:"records"`
}

// HasRefusals reports whether any record in the report was refused outright.
// VerdictPending and VerdictDeferred are not refusals: apply.go's pre-flight
// gate uses this, not a raw count of non-adopt verdicts, to decide whether it
// may proceed to the write phase.
func (rep Report) HasRefusals() bool {
	for _, r := range rep.Records {
		if r.Verdict == VerdictRefused {
			return true
		}
	}
	return false
}

// requestFor validates one record's own fields -- everything that does not
// need configuration, the ledger or the service to check -- and assembles
// the domain.Request Adopt/PlanAdoption take. Every field PlanAdoption and
// Adopt would otherwise derive (prefix_length, address_family, pool, domain)
// is deliberately absent from Record in the first place (read.go's column
// list), so there is nothing here to strip; this function only rejects a
// record whose own columns disagree with themselves (a non-canonical CIDR, a
// vpc row with a parent key, a subnet row without one).
func requestFor(r Record) (domain.Request, error) {
	switch {
	case r.TenantID == "":
		return domain.Request{}, errors.New("tenant_id is required")
	case r.AllocationKey == "":
		return domain.Request{}, errors.New("allocation_key is required")
	case r.Scope != "vpc" && r.Scope != "subnet":
		return domain.Request{}, fmt.Errorf("scope must be vpc or subnet, got %q", r.Scope)
	case r.Environment == "":
		return domain.Request{}, errors.New("environment is required")
	case r.Region == "":
		return domain.Request{}, errors.New("region is required")
	case r.AccountID == "":
		return domain.Request{}, errors.New("account_id is required")
	case r.ResourceID == "":
		return domain.Request{}, errors.New("resource_id is required")
	case r.NetBoxPrefixID == "":
		return domain.Request{}, errors.New("netbox_prefix_id is required")
	}
	p, err := netip.ParsePrefix(r.CIDR)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return domain.Request{}, fmt.Errorf("cidr %q is not a canonical IPv4 prefix", r.CIDR)
	}
	if r.Scope == "vpc" {
		if r.ParentAllocationKey != "" || r.AvailabilityZoneID != "" {
			return domain.Request{}, errors.New("a vpc record must not supply parent_allocation_key or availability_zone_id")
		}
	} else {
		if r.ParentAllocationKey == "" {
			return domain.Request{}, errors.New("a subnet record requires parent_allocation_key")
		}
		if r.AvailabilityZoneID == "" {
			return domain.Request{}, errors.New("a subnet record requires availability_zone_id")
		}
	}
	req := domain.Request{
		AllocationKey: r.AllocationKey,
		Scope:         r.Scope,
		Environment:   r.Environment,
		Region:        r.Region,
		AccountID:     r.AccountID,
		PrefixLength:  p.Bits(),
	}
	if r.Scope == "subnet" {
		req.AvailabilityZoneID = r.AvailabilityZoneID
	}
	return req, nil
}

// parentLookup resolves a subnet's parent_allocation_key to a real
// allocation id: either an already-committed allocation (seeded from the
// ledger, or recorded here as apply adopts each row), or -- if not
// committed anywhere yet -- a note that the key belongs to a vpc record
// elsewhere in this same file, so a plan verdict can say "deferred" instead
// of refusing a legitimate shape of input.
type parentLookup struct {
	resolved map[string]parentRef
	inFile   map[string]bool
}

// parentRef is what a subnet needs to know about its parent: the allocation
// id its request names, and whether the parent is bound -- the evidence the
// service demands before it exempts the parent VPC from the subnet's overlap
// rule (parentResourceMatches in internal/service).
type parentRef struct {
	id    string
	bound bool
}

func newParentLookup() *parentLookup {
	return &parentLookup{resolved: map[string]parentRef{}, inFile: map[string]bool{}}
}

const waitingForParentMessage = "parent allocation %q has no verified binding yet: an adopted VPC is born RESERVED, and its subnets become adoptable only once the owning team has tagged the VPC with both platform tags and the worker has observed it (the parent is then ACTIVE); re-run apply after that"

func parentLookupKey(tenant, key string) string { return tenant + "\x00" + key }

// seedFromLedger records every committed allocation of each given tenant, so
// a subnet whose parent was adopted (and, per ADR 0010, tagged and promoted
// to ACTIVE) in an earlier run resolves without that parent's row needing to
// appear in this file at all. It looks the tenant up with a principal that
// carries only TenantID, not one resolved from configuration: Service.List
// filters solely on Allocation.TenantID == Principal.TenantID, so the
// account/environment/region scope resolvePrincipal computes for a specific
// record would be irrelevant here and, worse, would arbitrarily pick one
// record's scope to stand for the whole tenant when several records of the
// same tenant target different scopes.
func (pl *parentLookup) seedFromLedger(ctx context.Context, svc Service, tenants []string) error {
	for _, tenant := range tenants {
		allocations, err := svc.List(ctx, domain.Principal{TenantID: tenant})
		if err != nil {
			return fmt.Errorf("listing allocations for tenant %q: %w", tenant, err)
		}
		for _, a := range allocations {
			pl.record(tenant, a.AllocationKey, a)
		}
	}
	return nil
}

// distinctTenants returns each tenant_id present in records, in first-seen
// order.
func distinctTenants(records []Record) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range records {
		if r.TenantID != "" && !seen[r.TenantID] {
			seen[r.TenantID] = true
			out = append(out, r.TenantID)
		}
	}
	return out
}

// noteFileRows records which allocation keys this table's own vpc rows
// declare, so resolve can tell "not adopted yet, but on its way in this run"
// apart from "not found anywhere".
func (pl *parentLookup) noteFileRows(records []Record) {
	for _, r := range records {
		if r.Scope == "vpc" {
			pl.inFile[parentLookupKey(r.TenantID, r.AllocationKey)] = true
		}
	}
}

// resolve looks up a parent by tenant and allocation key. found is false
// only when the key is neither an already-resolved allocation nor a vpc row
// present in this file at all -- the one case both plan and apply refuse
// outright, per docs/WORK_PLAN.md package F4 ("a subnet whose parent is not
// in the file and not in the ledger is refused").
func (pl *parentLookup) resolve(tenant, key string) (ref parentRef, deferred, found bool) {
	k := parentLookupKey(tenant, key)
	if ref, ok := pl.resolved[k]; ok {
		return ref, false, true
	}
	if pl.inFile[k] {
		return parentRef{}, true, true
	}
	return parentRef{}, false, false
}

// record stores a just-adopted (or replayed) allocation's id under its
// tenant and key, so a later row in the same apply run -- a subnet whose
// parent this same run adopted moments ago -- resolves it.
func (pl *parentLookup) record(tenant, key string, a domain.Allocation) {
	pl.resolved[parentLookupKey(tenant, key)] = parentRef{id: a.ID, bound: a.Binding != nil}
}

// codeAndMessage extracts an APIError's code and message for the report;
// anything else (a transport-level or context error) is reported under a
// generic code so the caller can still tell "the service refused this" apart
// from "something around the service failed".
func codeAndMessage(err error) (code, message string) {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code, apiErr.Message
	}
	return "adapter_error", err.Error()
}

// evaluateRecord produces one record's verdict: structural validation, then
// principal resolution, then (for a subnet) the parent lookup, then --
// unless one of those already refused or deferred it -- the service's own
// PlanAdoption. It never mutates the record's own request beyond what
// requestFor and the parent lookup already decided, and it never calls
// Adopt.
func evaluateRecord(ctx context.Context, svc Service, cfg domain.Config, r Record, pl *parentLookup, operator string) RecordResult {
	res := RecordResult{SourceRow: r.SourceRow, TenantID: r.TenantID, AllocationKey: r.AllocationKey, Scope: r.Scope}

	req, err := requestFor(r)
	if err != nil {
		res.Verdict, res.RefusalCode, res.Message = VerdictRefused, "invalid_request", err.Error()
		return res
	}

	if r.Scope == "subnet" {
		parent, deferred, found := pl.resolve(r.TenantID, r.ParentAllocationKey)
		if !found {
			res.Verdict, res.RefusalCode, res.Message = VerdictRefused, "invalid_parent",
				fmt.Sprintf("parent allocation key %q is neither an existing allocation nor another record in this file", r.ParentAllocationKey)
			return res
		}
		if deferred {
			res.Verdict, res.Message = VerdictDeferred,
				fmt.Sprintf("parent allocation key %q is adopted earlier in this same file; this subnet is not attempted in that run. "+waitingForParentMessage, r.ParentAllocationKey, r.ParentAllocationKey)
			return res
		}
		if !parent.bound {
			res.Verdict, res.Message = VerdictWaitingForParent, fmt.Sprintf(waitingForParentMessage, r.ParentAllocationKey)
			return res
		}
		req.ParentAllocationID = parent.id
	}

	principal, err := resolvePrincipal(cfg.Identities, r)
	if err != nil {
		res.Verdict, res.RefusalCode, res.Message = VerdictRefused, "invalid_request", err.Error()
		return res
	}

	pin := service.Adoption{Operator: operator, CIDR: r.CIDR, NetworkID: r.NetBoxPrefixID, ResourceID: r.ResourceID}
	v, err := svc.PlanAdoption(ctx, principal, req, pin)
	if err != nil {
		res.Verdict = VerdictRefused
		res.RefusalCode, res.Message = codeAndMessage(err)
		return res
	}

	res.CIDR = v.Allocation.CIDR
	res.PrefixLength = v.Allocation.PrefixLength
	res.PoolID = v.Allocation.PoolID
	res.DomainID = v.Allocation.DomainID
	res.AllocationID = v.Allocation.ID
	switch {
	case v.Existing && v.Pending:
		res.Verdict = VerdictPending
	case v.Existing:
		res.Verdict = VerdictWouldReplay
	default:
		res.Verdict = VerdictWouldAdopt
	}
	return res
}

// evaluateAll evaluates every record and returns the report in the given
// order; it never calls Adopt or writes to the ledger.
func evaluateAll(ctx context.Context, svc Service, cfg domain.Config, records []Record, pl *parentLookup, operator string) Report {
	rep := Report{Records: make([]RecordResult, len(records))}
	for i, r := range records {
		rep.Records[i] = evaluateRecord(ctx, svc, cfg, r, pl, operator)
	}
	return rep
}
