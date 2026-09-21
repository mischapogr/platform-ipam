package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Adoption turns occupancy into ownership: the one operation ADR 0007
// deliberately left out, and the one ADR 0010 adds. Ensure refuses to create
// over an existing prefix and EnsureOccupancy refuses to write the ownership
// fields at all, so without this the two halves never meet. Everything here is
// written so that the only prefix that can gain the ledger's markers is a
// single prefix at exactly the allocation's CIDR, in the pool's VRF, carrying
// the import tag and owned by nobody -- the object an operator reviewed.

// Refusals a caller can classify with errors.Is. There is no undo for an
// adoption (ADR 0010), so each of these is a refusal to write rather than a
// condition to work around.
var (
	ErrAdoptInvalid     = errors.New("invalid adoption")
	ErrAdoptNotFound    = errors.New("no prefix to adopt")
	ErrAdoptConflict    = errors.New("conflicting NetBox object")
	ErrAdoptManaged     = errors.New("refusing to adopt a prefix that is already owned")
	ErrAdoptNotImported = errors.New("refusing to adopt a prefix that does not carry the import tag")
	// ErrAdoptReadBack means the prefix was patched and then did not read back
	// as this allocation's. The write has already happened, so the caller leaves
	// the operation pending for a person rather than writing again.
	ErrAdoptReadBack = errors.New("adopted prefix did not read back as this allocation's")
)

// Adopt converts the imported prefix at the allocation's CIDR into a managed
// one and returns its NetBox ID. It performs no write until every check has
// passed, and a re-run after a crash converges on the prefix it already wrote
// instead of writing a second time.
func (c *Client) Adopt(ctx context.Context, a domain.Allocation, operationID string) (string, error) {
	if a.ID == "" || a.CIDR == "" || operationID == "" {
		return "", fmt.Errorf("%w: allocation ID, CIDR, and operation ID are required", ErrAdoptInvalid)
	}
	d, p, err := c.allocationDomain(a)
	if err != nil {
		return "", err
	}
	cidr, err := adoptCIDR(a.CIDR, p)
	if err != nil {
		return "", err
	}
	vrfID := poolVRF(d, p)
	existing, err := c.prefixesAt(ctx, cidr, vrfID)
	if err != nil {
		return "", uncertainAdopt("listing the prefixes at "+cidr.String(), err)
	}
	// A prefix in another VRF is invisible to Snapshot, so adopting it would
	// give the allocation an inventory ID the allocator never reads; it is not
	// found here rather than adopted elsewhere.
	switch {
	case len(existing) == 0:
		return "", fmt.Errorf("%w: no prefix holds %s in VRF %d", ErrAdoptNotFound, cidr, vrfID)
	case len(existing) > 1:
		return "", fmt.Errorf("%w: %d prefixes hold %s in VRF %d", ErrAdoptConflict, len(existing), cidr, vrfID)
	case existing[0].ID == 0:
		return "", fmt.Errorf("%w: NetBox returned a prefix without an ID", ErrAdoptConflict)
	}
	id := existing[0].ID
	current, etag, err := c.adoptRead(ctx, id)
	if err != nil {
		return "", uncertainAdopt(fmt.Sprintf("reading prefix %d", id), err)
	}
	if got, err := parsePrefix(current.Prefix); err != nil || got != cidr || current.vrfID() != vrfID {
		return "", fmt.Errorf("%w: prefix %d reads as %q in VRF %d, expected %s in VRF %d", ErrAdoptConflict, id, current.Prefix, current.vrfID(), cidr, vrfID)
	}

	// A crashed adoption leaves the ledger's intent and a prefix already
	// carrying our markers. Converging on it without writing is what makes the
	// operation re-runnable, and the test is the identity check Ensure's own
	// recovery applies -- a marker that is ours but names another operation is
	// still a refusal.
	if stringCF(current.CustomFields, allocationIDCF) == a.ID {
		if err := c.validateExisting(current, a, operationID, vrfID); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAdoptConflict, err)
		}
		if key := stringCF(current.CustomFields, allocationKeyCF); key != a.AllocationKey {
			return "", fmt.Errorf("%w: prefix %d carries %s %q, expected %q", ErrAdoptConflict, id, allocationKeyCF, key, a.AllocationKey)
		}
		return strconv.Itoa(id), nil
	}
	// Any other ownership field means the prefix is someone else's, or was half
	// written by an operation that is still open. Neither is ours to finish.
	for _, field := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		if value := stringCF(current.CustomFields, field); value != "" {
			return "", fmt.Errorf("%w: prefix %d carries %s %q", ErrAdoptManaged, id, field, value)
		}
	}
	if !current.imported() {
		return "", fmt.Errorf("%w: prefix %d is not tagged %s", ErrAdoptNotImported, id, ImportedTag)
	}
	// Ensure's guard, for the same reason: two prefixes claiming one allocation
	// make the inventory ID ambiguous for every later Sync and Delete.
	claimed, err := c.findByMarker(ctx, a.ID)
	if err != nil {
		return "", uncertainAdopt("searching for another prefix claiming allocation "+a.ID, err)
	}
	for _, other := range claimed {
		if other.ID != id {
			return "", fmt.Errorf("%w: prefix %d already claims allocation %s", ErrAdoptConflict, other.ID, a.ID)
		}
	}

	fields := ownedFields(a, operationID)
	for key, value := range current.CustomFields {
		if _, owned := fields[key]; !owned {
			fields[key] = value
		}
	}
	// The payload carries no "tags" key on purpose. A NetBox PATCH replaces the
	// tag list when it includes one and leaves it alone when it does not, so
	// omitting it is what keeps the import tag -- and with it the provenance of
	// occupancy that was never ours -- on the prefix. The unowned custom fields
	// copied above carry the import batch and source through for the same
	// reason, exactly as Sync preserves an operator's own fields.
	payload := map[string]any{"status": "reserved", "custom_fields": fields}
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}
	resp, err := c.requestWithHeaders(ctx, http.MethodPatch, adoptPath(id), payload, headers)
	if err != nil {
		var status *HTTPError
		if errors.As(err, &status) && status.Status == http.StatusPreconditionFailed {
			return "", fmt.Errorf("%w: prefix %d changed between the read and the write", ErrAdoptConflict, id)
		}
		return "", uncertainAdopt(fmt.Sprintf("patching prefix %d", id), err)
	}
	resp.Body.Close()

	// Re-read rather than trust the echo of the write: whether the markers
	// landed on the prefix that was reviewed is the question the whole
	// operation turns on, and it is answered by the validation guarding Ensure.
	after, _, err := c.adoptRead(ctx, id)
	if err != nil {
		return "", uncertainAdopt(fmt.Sprintf("reading prefix %d back", id), err)
	}
	if err := c.validateExisting(after, a, operationID, vrfID); err != nil {
		return "", fmt.Errorf("%w: %v", ErrAdoptReadBack, err)
	}
	if !after.imported() {
		return "", fmt.Errorf("%w: prefix %d no longer carries %s", ErrAdoptReadBack, id, ImportedTag)
	}
	return strconv.Itoa(id), nil
}

// unanswered reports a request error that is not an answer about the object: a
// transport failure or a timeout (no *HTTPError at all), a 5xx, or a 429. A 4xx
// is an answer -- NetBox read the request and refused it -- and repeating it
// will not change it, so it is never uncertain.
func unanswered(err error) bool {
	var status *HTTPError
	if !errors.As(err, &status) {
		return true
	}
	return status.Status >= 500 || status.Status == http.StatusTooManyRequests
}

// uncertainAdopt wraps an error Adopt cannot decide on. Where the adapter got
// no answer it carries domain.ErrInventoryUncertain, which is what the service
// retries on in silence; a 4xx keeps its cause and stays a refusal.
func uncertainAdopt(what string, err error) error {
	if unanswered(err) {
		return fmt.Errorf("%w: uncertain NetBox adoption (operation remains pending): %s: %w", domain.ErrInventoryUncertain, what, err)
	}
	return fmt.Errorf("NetBox refused the adoption (operation remains pending): %s: %w", what, err)
}

func adoptPath(id int) string { return "/api/ipam/prefixes/" + strconv.Itoa(id) + "/" }

// adoptRead fetches one prefix with its tags, and the ETag that lets the write
// that follows be conditional. NetBox 4.6.7 returns a weak ETag derived from
// last_updated and answers 412 to a PATCH whose If-Match no longer matches it
// (established against the development NetBox; see ADR 0010), which closes the
// window between these checks and the write rather than merely narrowing it.
// An installation that returns no ETag falls back to the unconditional write
// ADR 0010 designed, where the re-read is what catches a foreign change.
func (c *Client) adoptRead(ctx context.Context, id int) (prefix, string, error) {
	var x prefix
	resp, err := c.request(ctx, http.MethodGet, adoptPath(id), nil)
	if err != nil {
		return x, "", err
	}
	etag := resp.Header.Get("ETag")
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return x, "", err
	}
	if err := json.Unmarshal(data, &x); err != nil {
		return x, "", fmt.Errorf("decode netbox prefix %d: %w", id, err)
	}
	return x, etag, nil
}

// adoptCIDR applies the CIDR rules an import applied when the prefix was
// written (canonical IPv4, ADR 0007) and the containment rule Ensure applies to
// a create, plus one this operation adds: a pool's own prefix is never
// adoptable. Snapshot requires exactly one prefix per configured pool and
// reports it as the pool itself, so owning it would misreport the pool and take
// the domain's allocation offline.
func adoptCIDR(s string, p domain.Pool) (netip.Prefix, error) {
	cidr, err := occupancyPrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: %v", ErrAdoptInvalid, err)
	}
	pc, err := parsePrefix(p.CIDR)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pool %s CIDR: %w", p.ID, err)
	}
	pc = pc.Masked()
	if cidr == pc {
		return netip.Prefix{}, fmt.Errorf("%w: %s is pool %s itself", ErrAdoptInvalid, cidr, p.ID)
	}
	if !pc.Contains(cidr.Addr()) || cidr.Bits() < pc.Bits() {
		return netip.Prefix{}, fmt.Errorf("%w: %s is outside pool %s (%s)", ErrAdoptInvalid, cidr, p.ID, pc)
	}
	return cidr, nil
}
