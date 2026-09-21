package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Abandoning an adoption is the only operation in this package that takes
// ownership away from an object without deleting it. An adoption makes the
// ledger's intent durable before it touches NetBox, so a hold that can never
// commit may already have converted a prefix; ADR 0012 records that clearing it
// is the only thing that stops the estate from holding a prefix which claims an
// allocation the ledger no longer has. Everything here is written so that the
// only prefix that can lose the markers is the single prefix this allocation's
// own marker names, carrying this operation's id -- never an object an operator
// merely pointed at, and never one some other operation half wrote.

// Refusals a caller can classify with errors.Is. Everything but
// ErrAbandonUncertain is a decision this adapter reached on evidence it read,
// so repeating the call will not change the answer.
var (
	ErrAbandonInvalid = errors.New("invalid abandon")
	// ErrAbandonAmbiguous means several prefixes claim the allocation. Clearing
	// one of them would leave the other claiming an allocation about to be
	// deleted, which is the state this operation exists to prevent.
	ErrAbandonAmbiguous = errors.New("more than one prefix claims this allocation")
	// ErrAbandonNotOurs means the prefix carrying this allocation's marker was
	// written under a different operation. Another adoption may be finishing it
	// right now, and this abandon has no evidence about that one.
	ErrAbandonNotOurs  = errors.New("refusing to clear a prefix another operation wrote")
	ErrAbandonConflict = errors.New("conflicting NetBox object")
	// ErrAbandonReadBack means the prefix was cleared and then did not read back
	// as unowned imported occupancy. The write has already happened, so the
	// caller stops short of deleting the hold and leaves it for a person rather
	// than writing again.
	ErrAbandonReadBack = errors.New("cleared prefix did not read back as unowned occupancy")
	// ErrAbandonUncertain says nothing about the prefix: the call may or may not
	// have cleared it. The causing error is kept in the chain, so a caller
	// classifying a timeout or a cancellation the way internal/service's
	// uncertainInventoryError classifies Adopt's errors still sees it.
	ErrAbandonUncertain = errors.New("uncertain NetBox abandon")
)

// importedStatus is the status ensureOccupancyPrefix gives the occupancy an
// import creates, and therefore what a cleared prefix carries when no prior
// status was recorded.
const importedStatus = "active"

// prefixStatuses are the values NetBox 4.6.7 accepts for a prefix, read from
// OPTIONS /api/ipam/prefixes/ on the development stack (2026-09-20).
var prefixStatuses = map[string]bool{"container": true, "active": true, "reserved": true, "deprecated": true}

// AbandonAdoption clears the prefix an unfinished adoption converted and
// returns it to unmanaged imported occupancy. It writes nothing until the
// object has been found twice by this allocation's marker and agreed both times
// that it is this operation's, and it never deletes: the prefix an import wrote
// outlives the adoption that failed, exactly as it outlived the one that
// succeeded.
func (c *Client) AbandonAdoption(ctx context.Context, a domain.Allocation, operationID string, prior domain.PriorInventory) error {
	if a.ID == "" || operationID == "" {
		return fmt.Errorf("%w: allocation ID and operation ID are required", ErrAbandonInvalid)
	}
	// The marker search is the whole of the evidence, and it is deliberately
	// agnostic about VRF and CIDR. Two of the routes to a stuck adoption are
	// defined by the ownership fields having landed on an object nobody
	// reviewed (ADR 0012), so a search narrowed to the reviewed CIDR would miss
	// precisely the prefix that has to be cleared.
	claimed, err := c.findByMarker(ctx, a.ID)
	if err != nil {
		return uncertainAbandon("searching for the prefix claiming allocation "+a.ID, err)
	}
	switch {
	case len(claimed) == 0:
		// Nothing claims the allocation, so there is nothing to give back. An
		// abandon interrupted after its write re-runs into this, which is what
		// lets a second run reach the delete instead of refusing for ever.
		return nil
	case len(claimed) > 1:
		return fmt.Errorf("%w: %d prefixes claim allocation %s", ErrAbandonAmbiguous, len(claimed), a.ID)
	case claimed[0].ID == 0:
		return fmt.Errorf("%w: NetBox returned a prefix without an ID", ErrAbandonConflict)
	}
	id := claimed[0].ID
	if err := clearingOurOwnOperation(claimed[0], id, operationID); err != nil {
		return err
	}
	// The list and the detail are two reads. The detail carries the ETag that
	// makes the write conditional, and asking a second time is what stops a
	// prefix that changed hands between them from being cleared on the list's
	// word.
	current, etag, err := c.adoptRead(ctx, id)
	if err != nil {
		return uncertainAbandon(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got := stringCF(current.CustomFields, allocationIDCF); got != a.ID {
		if got == "" && !current.owned() {
			return nil
		}
		return fmt.Errorf("%w: prefix %d now carries %s %q, expected %q", ErrAbandonConflict, id, allocationIDCF, got, a.ID)
	}
	if err := clearingOurOwnOperation(current, id, operationID); err != nil {
		return err
	}

	// Exactly the keys an adoption of this allocation writes, emptied. Deriving
	// them from ownedFields rather than listing them is what keeps the two from
	// drifting when a field is added; sending JSON null is what NetBox 4.6.7
	// reads back as unset, measured against the development stack and recorded
	// in ADR 0012. Note what is *not* here: ownedFields omits
	// platform_aws_resource_id for an allocation with no binding, which every
	// uncommitted adoption is, so the value an import wrote there is neither
	// overwritten by Adopt nor emptied here.
	fields := map[string]any{}
	for key := range ownedFields(a, operationID) {
		fields[key] = nil
	}
	// The payload carries no "tags" key, for the reason Adopt carries none: a
	// PATCH replaces the tag list when it includes one and leaves it alone when
	// it does not, and the import tag is the provenance that makes this prefix
	// occupancy again rather than an orphan. No unowned custom field is sent
	// either -- custom fields merge key by key, so an omitted key is untouched.
	// Two of the keys an adoption owns were the import's before they were the
	// allocation's. Where the ledger recorded what they held, the clear puts
	// that back instead of leaving a null the import never wrote.
	for key, was := range map[string]string{awsAccountCF: prior.AccountID, awsRegionCF: prior.Region} {
		if _, owned := fields[key]; owned && was != "" {
			fields[key] = was
		}
	}
	payload := map[string]any{"status": abandonStatus(prior.Status), "custom_fields": fields}
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}
	resp, err := c.requestWithHeaders(ctx, http.MethodPatch, adoptPath(id), payload, headers)
	if err != nil {
		var status *HTTPError
		if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
			return fmt.Errorf("%w: prefix %d changed between the read and the write", ErrAbandonConflict, id)
		}
		return uncertainAbandon(fmt.Sprintf("clearing prefix %d", id), err)
	}
	resp.Body.Close()

	// Re-read rather than trust the echo of the write: whether this prefix
	// still claims the allocation is the question the caller's next step --
	// deleting the hold -- turns on.
	after, _, err := c.adoptRead(ctx, id)
	if err != nil {
		return uncertainAbandon(fmt.Sprintf("reading prefix %d back after clearing it", id), err)
	}
	if after.owned() {
		return fmt.Errorf("%w: prefix %d still carries an ownership field", ErrAbandonReadBack, id)
	}
	if !after.imported() {
		return fmt.Errorf("%w: prefix %d no longer carries %s", ErrAbandonReadBack, id, ImportedTag)
	}
	return nil
}

// clearingOurOwnOperation is the identity check that separates this abandon's
// half-converted prefix from one another operation wrote. The allocation marker
// alone is not enough: it is an operator who says which allocation is being
// withdrawn, and a prefix written under an operation that is still open belongs
// to that operation, not to this one.
func clearingOurOwnOperation(x prefix, id int, operationID string) error {
	if op := stringCF(x.CustomFields, operationIDCF); op != operationID {
		return fmt.Errorf("%w: prefix %d carries %s %q, this abandon is for %q", ErrAbandonNotOurs, id, operationIDCF, op, operationID)
	}
	return nil
}

// abandonStatus is the status the clear restores. ADR 0012 records the status a
// prefix carried before its adoption so that an operator's own value survives
// an adoption that is withdrawn; a hold that records none -- every hold created
// before that field existed -- falls back to the status an import writes. So
// does a value this NetBox would refuse, because the status is cosmetic --
// Snapshot reads none -- and must never be the reason a prefix keeps markers
// the ledger no longer backs.
func abandonStatus(prior string) string {
	if prefixStatuses[prior] {
		return prior
	}
	return importedStatus
}

// uncertainAbandon wraps an error that says nothing about the prefix. Both
// sentinels stay in the chain: ErrAbandonUncertain for a caller that asks this
// adapter's own question, and the cause for one classifying a timeout or a
// cancellation the way the worker already classifies Adopt's.
func uncertainAbandon(what string, err error) error {
	// ErrAbandonUncertain is this adapter's own name for "I could not decide".
	// domain.ErrInventoryUncertain is the narrower thing the service acts on:
	// no answer at all, so re-running is the only useful advice. A 4xx is an
	// answer, so it carries the first and not the second.
	if unanswered(err) {
		return fmt.Errorf("%w: %w: %s: %w", domain.ErrInventoryUncertain, ErrAbandonUncertain, what, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrAbandonUncertain, what, err)
}
