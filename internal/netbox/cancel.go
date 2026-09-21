package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Cancelling a reservation is the only operation in this package that destroys
// an object it found by a marker rather than by a stored inventory id. A
// reservation makes the ledger's intent durable before it touches NetBox, so a
// hold the platform has declared stuck may already have created its prefix, and
// ADR 0013 records that deleting that prefix is what stops the estate from
// holding an object which claims an allocation the ledger is about to remove.
// Deleting is also what separates this from AbandonAdoption, which clears and
// keeps: an adoption's prefix belongs to an import and outlives the adoption,
// while a reservation's prefix belongs to nothing else, so a cleared one would
// sit unowned and untagged at that CIDR and Ensure's own exact-CIDR guard would
// refuse every later reservation there -- the platform would poison its own
// pool while tidying up. Everything here is written so that the only prefix
// that can be deleted is the single prefix this allocation's own marker names,
// carrying this operation's id and no import tag: never an object a caller
// pointed at, never one another operation wrote, and never one an import
// created.

// Refusals a caller can classify with errors.Is. Everything but
// ErrCancelUncertain is a decision this adapter reached on evidence it read, so
// repeating the call will not change the answer.
var (
	ErrCancelInvalid = errors.New("invalid reservation cancel")
	// ErrCancelAmbiguous means several prefixes claim the allocation. Deleting
	// one of them would leave the other claiming an allocation about to be
	// deleted, which is the state this operation exists to prevent; it is also
	// the duplicate-marker condition Ensure refuses on, and the only fix is to
	// remove one of them by hand.
	ErrCancelAmbiguous = errors.New("more than one prefix claims this allocation")
	// ErrCancelNotOurs means the prefix carrying this allocation's marker was
	// written under a different operation. Another reservation may be finishing
	// it right now, and this cancel has no evidence about that one.
	ErrCancelNotOurs = errors.New("refusing to delete a prefix another operation wrote")
	// ErrCancelImported means the match carries the import tag. Ensure never
	// writes that tag, so a reservation's marker on an imported prefix is
	// evidence of an out-of-band edit rather than of a create this platform
	// performed -- and deleting it would destroy occupancy an import recorded
	// (ADR 0007), which no reservation ever owned.
	ErrCancelImported = errors.New("refusing to delete a prefix that carries the import tag")
	ErrCancelConflict = errors.New("conflicting NetBox object")
	// ErrCancelReadBack means the delete was sent and the prefix was still
	// there afterwards. Something answered for an object that should be gone,
	// so the caller stops short of deleting the hold and leaves it for a person
	// rather than writing again.
	ErrCancelReadBack = errors.New("prefix is still present after the delete was sent")
	// ErrCancelUncertain says nothing about the prefix: the call may or may not
	// have deleted it. The causing error is kept in the chain, so a caller
	// classifying a timeout or a cancellation the way internal/service's
	// uncertainInventoryError classifies Ensure's errors still sees it.
	ErrCancelUncertain = errors.New("uncertain NetBox reservation cancel")
)

// CancelReservation deletes the prefix a reservation that can never complete
// created, so that the hold can be removed without leaving an object claiming
// an allocation the ledger no longer has. It writes nothing until the object
// has been found twice by this allocation's marker and agreed both times that
// it is this operation's and not an import's, and it confirms absence by
// reading the object back, as Delete does.
//
// NetBox neither refuses nor cascades the delete of a prefix that contains
// other prefixes: nesting is computed from the CIDR and is not a relation the
// delete follows, measured against the development NetBox 4.6.7 and recorded in
// ADR 0013. So there is no child to protect and no child check here, and a
// prefix nested inside a cancelled reservation's is left exactly as it was.
func (c *Client) CancelReservation(ctx context.Context, a domain.Allocation, operationID string) error {
	if a.ID == "" || operationID == "" {
		return fmt.Errorf("%w: allocation ID and operation ID are required", ErrCancelInvalid)
	}
	// The marker search is the whole of the evidence, and it is deliberately
	// agnostic about VRF and CIDR. A reservation's prefix can be left behind by
	// a create whose commit was lost and by a validateExisting refusal over a
	// changed pool VRF (ADR 0013), so a search narrowed to the held CIDR in the
	// configured VRF would miss precisely the prefix that has to go.
	claimed, err := c.findByMarker(ctx, a.ID)
	if err != nil {
		return uncertainCancel("searching for the prefix claiming allocation "+a.ID, err)
	}
	switch {
	case len(claimed) == 0:
		// Nothing claims the allocation, so there is nothing to delete. This is
		// the commonest outcome, because the commonest refusal behind a stuck
		// reservation is the exact-CIDR guard that runs before the create; it
		// is also what a cancel interrupted after its delete re-runs into,
		// which is what lets a second run reach the ledger delete instead of
		// refusing for ever.
		return nil
	case len(claimed) > 1:
		return fmt.Errorf("%w: %d prefixes claim allocation %s", ErrCancelAmbiguous, len(claimed), a.ID)
	case claimed[0].ID == 0:
		return fmt.Errorf("%w: NetBox returned a prefix without an ID", ErrCancelConflict)
	}
	id := claimed[0].ID
	if err := deletingOurOwnReservation(claimed[0], id, operationID); err != nil {
		return err
	}
	// The list and the detail are two reads. The detail carries the ETag that
	// makes the delete conditional, and asking a second time is what stops a
	// prefix that changed hands between them from being destroyed on the list's
	// word.
	current, etag, err := c.adoptRead(ctx, id)
	if err != nil {
		return uncertainCancel(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got := stringCF(current.CustomFields, allocationIDCF); got != a.ID {
		if got == "" && !current.owned() {
			// The marker is gone and nothing else claims the object, so no
			// prefix carries this allocation's markers -- the question this
			// call answers -- and what is left is not ours to delete.
			return nil
		}
		return fmt.Errorf("%w: prefix %d now carries %s %q, expected %q", ErrCancelConflict, id, allocationIDCF, got, a.ID)
	}
	if err := deletingOurOwnReservation(current, id, operationID); err != nil {
		return err
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
		// Measured against NetBox 4.6.7: a DELETE whose If-Match no longer
		// matches the weak ETag answers 412 and leaves the object in place
		// (ADR 0013). Whatever changed the prefix has to be looked at before
		// anything is deleted, so this is a refusal and never a retry.
		return fmt.Errorf("%w: prefix %d changed between the read and the delete", ErrCancelConflict, id)
	case answeredWith(err, http.StatusNotFound):
		// The same NetBox answers 404 to a DELETE of an id that is already
		// gone. Somebody removed the object between the read and the write,
		// which is the outcome this call was asking for, so the confirmation
		// below decides rather than this line.
	default:
		return uncertainCancel(fmt.Sprintf("deleting prefix %d", id), err)
	}

	// Confirm absence by reading it back rather than trusting the status of the
	// write, exactly as Delete does: whether anything still claims the
	// allocation is the question the caller's next step -- removing the hold
	// from the ledger -- turns on.
	if _, _, err := c.adoptRead(ctx, id); err == nil {
		return fmt.Errorf("%w: prefix %d still reads back", ErrCancelReadBack, id)
	} else if !answeredWith(err, http.StatusNotFound) {
		return uncertainCancel(fmt.Sprintf("reading prefix %d back after deleting it", id), err)
	}
	return nil
}

// deletingOurOwnReservation is the identity check that separates this
// reservation's own prefix from one something else wrote. The allocation marker
// alone is not enough on either count: a prefix written under an operation that
// is still open belongs to that operation, and a prefix carrying the import tag
// was created by the onboarding import rather than by Ensure, which writes no
// tag at all -- so a reservation's marker on it is evidence of an out-of-band
// edit and never of a create this platform performed.
func deletingOurOwnReservation(x prefix, id int, operationID string) error {
	if op := stringCF(x.CustomFields, operationIDCF); op != operationID {
		return fmt.Errorf("%w: prefix %d carries %s %q, this cancel is for %q", ErrCancelNotOurs, id, operationIDCF, op, operationID)
	}
	if x.imported() {
		return fmt.Errorf("%w: prefix %d is tagged %s", ErrCancelImported, id, ImportedTag)
	}
	return nil
}

// answeredWith reports that NetBox answered this request with exactly the given
// status. A transport failure or a timeout carries no *HTTPError and is
// therefore never one of them.
func answeredWith(err error, status int) bool {
	var answer *HTTPError
	return errors.As(err, &answer) && answer.Status == status
}

// uncertainCancel wraps an error that says nothing about the prefix. Both
// sentinels stay in the chain: ErrCancelUncertain for a caller that asks this
// adapter's own question, and domain.ErrInventoryUncertain -- the narrower
// thing the service acts on -- only where the request got no answer at all. A
// 4xx is an answer, so it carries the first and not the second.
func uncertainCancel(what string, err error) error {
	if unanswered(err) {
		return fmt.Errorf("%w: %w: %s: %w", domain.ErrInventoryUncertain, ErrCancelUncertain, what, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrCancelUncertain, what, err)
}
