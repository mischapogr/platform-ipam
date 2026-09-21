package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// ADR 0016's additive refresh (package M9b3): onboard apply --refresh, the
// first update path EnsureOccupancy has ever had. Everything here writes
// only platform_import_contributors, platform_import_source and, once, the
// reconstructed marker -- never the description, tags, status, tenant or any
// other operator-owned field -- and only for a prefix internal/onboard.Plan
// already reports RuleAlreadyUnmanaged for. It never creates: EnsureOccupancy
// is the only creator of an imported prefix.

// ImportContributorsReconstructedField is the top-level marker ADR 0016
// requires beside the contributor list, never inside an entry: "a prefix
// with no list gains one that is flagged reconstructed... the two cannot be
// distinguished afterwards, and a removal must not treat them alike." A
// top-level custom field lets a later removal (package M9b4) refuse a
// reconstructed list without reading a single entry. It is unowned, exactly
// like ImportContributorsField, so an adoption's merge preserves it and an
// abandonment's clear leaves it alone.
const ImportContributorsReconstructedField = "platform_import_contributors_reconstructed"

// Actions reported by RefreshOccupancy.
const (
	RefreshWritten   = "written"
	RefreshUnchanged = "unchanged"
)

// RefreshResult reports what one refresh call did.
type RefreshResult struct {
	ID            string // NetBox object ID
	Action        string // RefreshWritten or RefreshUnchanged
	Reconstructed bool   // true when this call populated a previously absent list
}

// Refusals a caller can classify with errors.Is. Like Adopt and
// AbandonAdoption, every one of these is a decision reached on evidence this
// adapter read, so repeating the call will not change the answer -- except a
// 412, which ErrOccupancyConflict already covers.
var (
	// ErrRefreshNotFound means no prefix holds the CIDR any more. A refresh
	// is reached only for a CIDR Plan just found as unmanaged occupancy in a
	// fresh snapshot, so this is a race with something else, not a mistyped
	// CIDR, and it is reported rather than silently skipped.
	ErrRefreshNotFound = errors.New("no prefix to refresh")
	// ErrRefreshUnreadable means the prefix's contributor list holds
	// something this version cannot decode: an operator's hand edit, or a
	// shape a later version wrote. ADR 0016: "never overwrite what cannot be
	// read" -- so this is unconditional, with no override.
	ErrRefreshUnreadable = errors.New("refusing to refresh a prefix whose contributor list is unreadable")
	// ErrRefreshReadBack means the write happened and the prefix then did
	// not read back as the unmanaged, imported occupancy carrying exactly
	// the list this call computed. The write has already happened, so the
	// caller does not retry; the entry stands as read, for a person to look
	// at.
	ErrRefreshReadBack = errors.New("refreshed prefix did not read back as expected")
)

// RefreshOccupancy additively updates the contributor list on the one
// prefix already occupying cidr in the domain's VRF. contributors is the
// table's own would-be list for this CIDR (onboard.ContributorsForRows,
// called by the caller exactly as EnsureOccupancy's caller already builds
// Occupancy.Contributors) -- RefreshOccupancy itself never reads a table row.
//
// The write happens only when the identity SET grows against what the
// prefix already carries, or the prefix carried no list at all
// (mergeContributors implements the rule; see its own comment). A second
// call with an unchanged table therefore makes no NetBox write, even when
// every contributor's observed_at in the table is newer than what is
// stored -- ADR 0014's amendment against a description-refresh on every
// collector run, applied here to the structured field it warned about.
func (c *Client) RefreshOccupancy(ctx context.Context, d domain.Domain, cidr string, source string, contributors []domain.Contributor) (RefreshResult, error) {
	cfg, _, err := c.occupancyDomain(d)
	if err != nil {
		return RefreshResult{}, err
	}
	prefixCIDR, err := occupancyPrefix(cidr)
	if err != nil {
		return RefreshResult{}, err
	}
	for i, ctr := range contributors {
		if ctr.Identity == "" {
			return RefreshResult{}, fmt.Errorf("%w: contributor %d has no identity", ErrOccupancyInvalid, i)
		}
	}
	vrfID := cfg.Backend.VRFID
	existingPrefixes, err := c.prefixesAt(ctx, prefixCIDR, vrfID)
	if err != nil {
		return RefreshResult{}, err
	}
	switch {
	case len(existingPrefixes) == 0:
		return RefreshResult{}, fmt.Errorf("%w: no prefix holds %s in VRF %d", ErrRefreshNotFound, prefixCIDR, vrfID)
	case len(existingPrefixes) > 1:
		return RefreshResult{}, fmt.Errorf("%w: %d prefixes hold %s in VRF %d", ErrOccupancyConflict, len(existingPrefixes), prefixCIDR, vrfID)
	}
	id := existingPrefixes[0].ID
	current, etag, err := c.adoptRead(ctx, id)
	if err != nil {
		return RefreshResult{}, uncertainRefresh(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got, err := parsePrefix(current.Prefix); err != nil || got != prefixCIDR || current.vrfID() != vrfID {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d reads as %q in VRF %d, expected %s in VRF %d", ErrOccupancyConflict, id, current.Prefix, current.vrfID(), prefixCIDR, vrfID)
	}
	// Any ownership field, not the import tag, is the refusal: an adopted
	// prefix keeps the import tag (ADR 0010, ADR 0012), so testing the tag
	// alone would let a refresh write straight into the allocation path.
	if current.owned() {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d is owned, refusing to refresh its contributor list", ErrOccupancyManaged, id)
	}
	// Defence in depth beyond what ADR 0016's refresh bullet list names: a
	// refresh is reached only for a CIDR Plan already found as unmanaged
	// occupancy, and every unmanaged prefix EnsureOccupancy has ever created
	// carries the import tag, so this should never fire in practice. It is
	// kept because "not owned" is not the same claim as "ours to touch",
	// and a hand-created NetBox prefix that happens to sit at an
	// already-unmanaged CIDR is not evidence this project wrote.
	if !current.imported() {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d is not tagged %s", ErrOccupancyManaged, id, ImportedTag)
	}
	existing, unreadable := contributorsCF(current.CustomFields, ImportContributorsField)
	if unreadable {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d", ErrRefreshUnreadable, id)
	}
	reconstruct := existing == nil

	merged, changed := mergeContributors(existing, contributors)
	if !changed {
		return RefreshResult{ID: strconv.Itoa(id), Action: RefreshUnchanged}, nil
	}
	// A reconstruction with nothing to reconstruct writes nothing, matching
	// EnsureOccupancy's create path: "an empty list is not an argument that
	// nothing contributes; it is a list nobody wrote" (ADR 0016). This is
	// defensive -- Plan raises RuleAlreadyUnmanaged only for a CIDR the
	// table's own rows produced, so contributors is never empty when this
	// path is reached in practice.
	if reconstruct && len(merged) == 0 {
		return RefreshResult{ID: strconv.Itoa(id), Action: RefreshUnchanged}, nil
	}

	fields := map[string]any{ImportContributorsField: merged, ImportSourceField: source}
	if reconstruct {
		fields[ImportContributorsReconstructedField] = true
	}
	if err := refuseManagedFields(fields); err != nil {
		return RefreshResult{}, err
	}
	payload := map[string]any{"custom_fields": fields}
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}
	resp, err := c.requestWithHeaders(ctx, http.MethodPatch, adoptPath(id), payload, headers)
	if err != nil {
		var status *HTTPError
		if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
			// A 412 is a refusal, not a retry (ADR 0010, ADR 0012, applied
			// here): the prefix may have just been adopted, and a blind
			// retry could write a contributor list onto an allocation.
			return RefreshResult{}, fmt.Errorf("%w: prefix %d changed between the read and the write", ErrOccupancyConflict, id)
		}
		return RefreshResult{}, uncertainRefresh(fmt.Sprintf("refreshing prefix %d", id), err)
	}
	resp.Body.Close()

	// Re-read rather than trust the echo of the write, exactly as Adopt and
	// AbandonAdoption do: whether the list that landed is the one this call
	// computed, on a prefix still unowned and still tagged, is the question
	// the whole call turns on.
	after, _, err := c.adoptRead(ctx, id)
	if err != nil {
		return RefreshResult{}, uncertainRefresh(fmt.Sprintf("reading prefix %d back after refreshing it", id), err)
	}
	if after.owned() || !after.imported() {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d no longer reads as unmanaged imported occupancy", ErrRefreshReadBack, id)
	}
	gotAfter, unreadableAfter := contributorsCF(after.CustomFields, ImportContributorsField)
	if unreadableAfter || !reflect.DeepEqual(gotAfter, merged) {
		return RefreshResult{}, fmt.Errorf("%w: prefix %d contributor list did not read back as written", ErrRefreshReadBack, id)
	}
	return RefreshResult{ID: strconv.Itoa(id), Action: RefreshWritten, Reconstructed: reconstruct}, nil
}

// mergeContributors implements ADR 0016's set-change rule: "a write happens
// when the contributor set changes, and never because an observation time
// changed."
//
// existing is what the prefix already carries -- nil means no list at all
// (package M9b3's reconstruct case), a non-nil, possibly empty slice means a
// real, already-written list. want is the table's own would-be list for
// this CIDR, built by the caller from onboard.ContributorsForRows.
//
// For an identity present in both, only observed_at and last_seen_batch are
// taken from want; first_seen_batch and every other field (account, region,
// type, resource id, parent, association, source file/row) are kept from
// existing, because they describe the sighting that first recorded this
// identity, not this one. For an identity in want only, the whole entry is
// added. No entry existing carries is ever removed.
//
// A degenerate identity -- the string a row with no resource_id column
// produces (docs/WORK_PLAN.md's M9b1 review note, and ADR 0016's own words:
// "match only itself... never counting as evidence") -- is handled by the
// same map keyed on Identity that every other entry goes through: several
// want rows sharing one degenerate identity collapse to the ONE map entry
// that identity string owns, so a refresh can never multiply it into
// several stored entries, matching or not.
//
// The merged list is returned sorted by (source file, source row, identity)
// -- the same order onboard.ContributorsForRows uses on create -- so the
// list's order is a function of its contents, not of how many refreshes
// produced it.
func mergeContributors(existing, want []domain.Contributor) (merged []domain.Contributor, changed bool) {
	reconstruct := existing == nil
	byIdentity := make(map[string]domain.Contributor, len(existing)+len(want))
	for _, c := range existing {
		byIdentity[c.Identity] = c
	}
	grew := false
	for _, w := range want {
		prior, existed := byIdentity[w.Identity]
		if !existed {
			grew = true
			byIdentity[w.Identity] = w
			continue
		}
		next := prior
		// A table without an observation time (a pre-M1b1 file) says nothing
		// about when the resource was seen, so it never erases a time an
		// earlier import recorded: a null is not an observation, and a
		// contributor that has one must not lose it and with it its standing
		// as removal evidence.
		if w.ObservedAt != nil {
			next.ObservedAt = w.ObservedAt
		}
		next.LastSeenBatch = w.LastSeenBatch
		byIdentity[w.Identity] = next
	}
	if !reconstruct && !grew {
		return nil, false
	}
	out := make([]domain.Contributor, 0, len(byIdentity))
	for _, c := range byIdentity {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SourceFile != out[j].SourceFile {
			return out[i].SourceFile < out[j].SourceFile
		}
		if out[i].SourceRow != out[j].SourceRow {
			return out[i].SourceRow < out[j].SourceRow
		}
		return out[i].Identity < out[j].Identity
	})
	return out, true
}

// uncertainRefresh wraps a request error RefreshOccupancy cannot decide
// from, the same way uncertainAdopt and uncertainEnsure already do for their
// own packages: no answer at all (a transport failure, a timeout, a 5xx, a
// 429) carries domain.ErrInventoryUncertain, which is the sentinel the
// worker's own classifier acts on; onboard apply has no such classifier and
// simply reports the error and stops, but the sentinel costs nothing to
// carry and keeps this call consistent with its siblings.
func uncertainRefresh(what string, err error) error {
	if unanswered(err) {
		return fmt.Errorf("%w: uncertain NetBox refresh: %s: %w", domain.ErrInventoryUncertain, what, err)
	}
	return fmt.Errorf("NetBox refused the refresh: %s: %w", what, err)
}
