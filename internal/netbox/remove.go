package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// ADR 0016's removal (package M9b4): onboard remove is the first command in
// this repository that deletes something the platform does not own. Two
// calls live here, and they read the ONE prefix removal names directly --
// prefixesAt plus adoptRead, exactly as RefreshOccupancy, Adopt,
// AbandonAdoption and CancelReservation each read their own single target --
// rather than through Snapshot, which lists an entire VRF and carries
// neither an ETag nor (until this file added it to the prefix type) the
// description.
//
// Neither call decides ADR 0016's evidence rules 1 (every contributor
// known), 2 (coverage complete for every contributing account and region) or
// 3 (every contributor observed absent): both need internal/assess's Report
// over the evidence collection being acted on, which this package does not
// import and has no reason to -- onboard remove's own command
// (internal/onboardcmd) is where a NetBox read and an assess Report meet.
// What lives here is ReadOccupancy, a plain read of what the prefix itself
// currently says, and DeleteOccupancy, which re-derives every refusal this
// package CAN decide on its own -- owned, not imported, a pool's own space,
// an unreadable or reconstructed list -- from a fresh read immediately
// before the delete, exactly as ADR 0016 requires ("verify every refusal
// again on the detail read"), and never trusts the caller's evidence pass to
// have been right a moment ago.

// Refusals a caller can classify with errors.Is. Every one but the two
// "uncertain" sentinels is a decision reached on evidence this adapter read,
// so repeating the call will not change the answer.
var (
	// ErrRemovalNotFound means no prefix holds the CIDR any more, mirroring
	// ErrRefreshNotFound: a removal is reached only for a CIDR the caller's
	// own dry run or Snapshot already reported as unmanaged imported
	// occupancy, so this is a race with something else, not a mistyped CIDR.
	ErrRemovalNotFound = errors.New("no prefix holds this CIDR")
	// ErrRemovalReconstructed means the prefix carries the permanent marker a
	// refresh sets when it populates a list on a prefix that had none: "the
	// two cannot be distinguished afterwards, and a removal must not treat
	// them alike" (ADR 0016).
	ErrRemovalReconstructed = errors.New("refusing to remove a prefix whose contributor list was reconstructed rather than written by the import that created it")
	// ErrRemovalIDMismatch means the NetBox id an --apply call was given does
	// not match the id a fresh read finds at this CIDR: the dry run's review
	// is stale, and a removal never trusts a caller's memory of an id over
	// what NetBox answers right now.
	ErrRemovalIDMismatch = errors.New("the NetBox id given to --apply does not match the prefix now at this CIDR")
	// ErrRemovalReadBack means the delete was sent and the prefix still read
	// back afterwards -- the same shape Client.Delete and CancelReservation
	// use: something answered for an object that should be gone, so the
	// caller stops short of reporting success rather than writing again.
	ErrRemovalReadBack = errors.New("prefix is still present after the delete was sent")
	// ErrRemovalUncertain says nothing about the prefix: the call may or may
	// not have deleted it.
	ErrRemovalUncertain = errors.New("uncertain NetBox removal")
)

// OccupancyDetail is what ReadOccupancy learns about the one prefix at cidr.
// It carries no verdict: internal/onboardcmd combines this with an assess
// Report to decide ADR 0016's evidence rules, and DeleteOccupancy performs
// its own independent re-checks from a fresh read rather than trusting a
// caller's copy of this value.
type OccupancyDetail struct {
	ID          string
	CIDR        string
	Description string
	Owned       bool
	Imported    bool
	// Contributors, Unreadable and Reconstructed carry exactly
	// domain.Network's own three-way distinction (nil list, unreadable list,
	// reconstructed list): see domain.Network's doc comments for why each is
	// kept apart from the others. A nil Contributors with Unreadable false
	// means no list at all; a non-nil, zero-length Contributors means the
	// field holds an empty JSON array -- ADR 0016 is explicit that the two
	// are not the same thing ("an empty list is not an argument that
	// nothing contributes; it is a list nobody wrote").
	Contributors  []domain.Contributor
	Unreadable    bool
	Reconstructed bool
	// PoolConflict is refusePoolPrefix's message when cidr equals a
	// configured pool's own CIDR or contains one as an ancestor, and empty
	// otherwise. It is carried here rather than left for the caller to
	// re-derive because refusePoolPrefix is unexported and onboard remove's
	// dry run needs this refusal reported alongside every other one, not
	// only rediscovered when --apply reaches DeleteOccupancy's own check.
	PoolConflict string
}

// ReadOccupancy reads the one prefix currently occupying cidr in the
// domain's VRF. It is read-only: unlike RefreshOccupancy and DeleteOccupancy
// it refuses nothing about what it finds, because onboard remove's dry run
// needs to report every evidence rule's outcome, not stop at the first
// refusal -- an operator reading the dry run should see the whole picture in
// one report rather than fixing one problem at a time.
func (c *Client) ReadOccupancy(ctx context.Context, d domain.Domain, cidr string) (OccupancyDetail, error) {
	cfg, pools, err := c.occupancyDomain(d)
	if err != nil {
		return OccupancyDetail{}, err
	}
	prefixCIDR, err := occupancyPrefix(cidr)
	if err != nil {
		return OccupancyDetail{}, err
	}
	var poolConflict string
	if err := refusePoolPrefix(prefixCIDR, pools); err != nil {
		poolConflict = err.Error()
	}
	vrfID := cfg.Backend.VRFID
	existing, err := c.prefixesAt(ctx, prefixCIDR, vrfID)
	if err != nil {
		return OccupancyDetail{}, err
	}
	switch {
	case len(existing) == 0:
		return OccupancyDetail{}, fmt.Errorf("%w: %s in VRF %d", ErrRemovalNotFound, prefixCIDR, vrfID)
	case len(existing) > 1:
		return OccupancyDetail{}, fmt.Errorf("%w: %d prefixes hold %s in VRF %d", ErrOccupancyConflict, len(existing), prefixCIDR, vrfID)
	}
	id := existing[0].ID
	current, _, err := c.adoptRead(ctx, id)
	if err != nil {
		return OccupancyDetail{}, uncertainRemoval(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got, err := parsePrefix(current.Prefix); err != nil || got != prefixCIDR || current.vrfID() != vrfID {
		return OccupancyDetail{}, fmt.Errorf("%w: prefix %d reads as %q in VRF %d, expected %s in VRF %d", ErrOccupancyConflict, id, current.Prefix, current.vrfID(), prefixCIDR, vrfID)
	}
	contributors, unreadable := contributorsCF(current.CustomFields, ImportContributorsField)
	return OccupancyDetail{
		ID: strconv.Itoa(id), CIDR: prefixCIDR.String(), Description: current.Description,
		Owned: current.owned(), Imported: current.imported(),
		Contributors: contributors, Unreadable: unreadable,
		Reconstructed: boolCF(current.CustomFields, ImportContributorsReconstructedField),
		PoolConflict:  poolConflict,
	}, nil
}

// RemoveResult reports what one DeleteOccupancy call did.
type RemoveResult struct {
	ID   string
	CIDR string
}

// DeleteOccupancy deletes the prefix netboxID names at cidr, in the shape
// ADR 0016 requires and Client.Delete already established for a
// platform-owned prefix: read the id back with its CIDR, verify every
// refusal again on the detail read, delete conditional on the ETag, and
// confirm by a re-read that answers 404 -- never by CIDR, never a filter
// (package M9b1 measured that NetBox silently ignores a filter on the
// contributor field, so a delete-by-filter could not be trusted to name one
// object).
//
// Every refusal this package can decide on its own is re-checked here from
// a FRESH read, independent of whatever internal/onboardcmd's dry run found
// a moment ago: owned, not imported, a pool's own space, an unreadable or
// reconstructed contributor list, and an empty contributor list ("an empty
// list is not an argument that nothing contributes; it is a list nobody
// wrote", ADR 0016). Evidence rules 2 and 3 (coverage, observed absence) are
// NOT re-checked here -- this package has no way to, since they need an
// assess Report -- so the caller must have already refused on them before
// ever calling this; DeleteOccupancy trusts the caller for those two only.
func (c *Client) DeleteOccupancy(ctx context.Context, d domain.Domain, cidr string, netboxID string) (RemoveResult, error) {
	cfg, pools, err := c.occupancyDomain(d)
	if err != nil {
		return RemoveResult{}, err
	}
	prefixCIDR, err := occupancyPrefix(cidr)
	if err != nil {
		return RemoveResult{}, err
	}
	if err := refusePoolPrefix(prefixCIDR, pools); err != nil {
		return RemoveResult{}, err
	}
	vrfID := cfg.Backend.VRFID
	existing, err := c.prefixesAt(ctx, prefixCIDR, vrfID)
	if err != nil {
		return RemoveResult{}, err
	}
	switch {
	case len(existing) == 0:
		return RemoveResult{}, fmt.Errorf("%w: %s in VRF %d", ErrRemovalNotFound, prefixCIDR, vrfID)
	case len(existing) > 1:
		return RemoveResult{}, fmt.Errorf("%w: %d prefixes hold %s in VRF %d", ErrOccupancyConflict, len(existing), prefixCIDR, vrfID)
	}
	id := existing[0].ID
	if strconv.Itoa(id) != netboxID {
		return RemoveResult{}, fmt.Errorf("%w: dry run named %s, NetBox now answers %d", ErrRemovalIDMismatch, netboxID, id)
	}
	current, etag, err := c.adoptRead(ctx, id)
	if err != nil {
		return RemoveResult{}, uncertainRemoval(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got, err := parsePrefix(current.Prefix); err != nil || got != prefixCIDR || current.vrfID() != vrfID {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d reads as %q in VRF %d, expected %s in VRF %d", ErrOccupancyConflict, id, current.Prefix, current.vrfID(), prefixCIDR, vrfID)
	}
	// Any ownership field, not the import tag, exactly as RefreshOccupancy
	// checks it: an adopted prefix keeps the import tag (ADR 0010, ADR
	// 0012), so testing the tag alone would let a removal delete an
	// allocation's own prefix.
	if current.owned() {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d is owned, refusing to remove it", ErrOccupancyManaged, id)
	}
	if !current.imported() {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d is not tagged %s, refusing to remove it", ErrOccupancyManaged, id, ImportedTag)
	}
	contributors, unreadable := contributorsCF(current.CustomFields, ImportContributorsField)
	if unreadable {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d", ErrRefreshUnreadable, id)
	}
	if len(contributors) == 0 {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d carries no contributor list", ErrOccupancyInvalid, id)
	}
	if boolCF(current.CustomFields, ImportContributorsReconstructedField) {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d", ErrRemovalReconstructed, id)
	}

	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}
	resp, err := c.requestWithHeaders(ctx, http.MethodDelete, adoptPath(id), nil, headers)
	switch {
	case err == nil:
		resp.Body.Close()
	case answeredWith(err, http.StatusPreconditionFailed):
		// A 412 is a refusal, not a retry, exactly as Adopt, AbandonAdoption
		// and RefreshOccupancy all decide: the prefix may have just been
		// adopted, and a blind retry could delete an allocation's own
		// object.
		return RemoveResult{}, fmt.Errorf("%w: prefix %d changed between the read and the delete", ErrOccupancyConflict, id)
	case answeredWith(err, http.StatusNotFound):
		// Something else already removed it, which is the outcome this call
		// was asking for; the confirmation read below decides, not this
		// line.
	default:
		return RemoveResult{}, uncertainRemoval(fmt.Sprintf("deleting prefix %d", id), err)
	}

	if _, _, err := c.adoptRead(ctx, id); err == nil {
		return RemoveResult{}, fmt.Errorf("%w: prefix %d still reads back", ErrRemovalReadBack, id)
	} else if !answeredWith(err, http.StatusNotFound) {
		return RemoveResult{}, uncertainRemoval(fmt.Sprintf("reading prefix %d back after removing it", id), err)
	}
	return RemoveResult{ID: strconv.Itoa(id), CIDR: prefixCIDR.String()}, nil
}

// uncertainRemoval wraps an error this package cannot decide from, the same
// way uncertainCancel and uncertainRefresh already do for their own
// packages: no answer at all carries domain.ErrInventoryUncertain, and
// ErrRemovalUncertain lets a caller classify "this adapter's own removal
// call got no answer" without depending on the narrower sentinel.
func uncertainRemoval(what string, err error) error {
	if unanswered(err) {
		return fmt.Errorf("%w: %w: %s: %w", domain.ErrInventoryUncertain, ErrRemovalUncertain, what, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrRemovalUncertain, what, err)
}
