package netbox

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// reservedPrefix is what Ensure leaves behind for a reservation: the CIDR in
// the pool's VRF, status reserved, this allocation's ownership fields derived
// from ownedFields so the fixture cannot drift from what a create writes, and
// no tags at all -- the absence of the import tag is what separates the object
// this platform created from the occupancy an import recorded.
func reservedPrefix(id int, a domain.Allocation, operationID string, vrfID int) map[string]any {
	fields := map[string]any{}
	for key, value := range ownedFields(a, operationID) {
		fields[key] = value
	}
	return map[string]any{
		"id": id, "prefix": a.CIDR, "vrf": map[string]any{"id": vrfID},
		"status": map[string]any{"value": "reserved", "label": "Reserved"},
		"tags":   []any{}, "custom_fields": fields,
	}
}

func cancelStack() (*adoptStack, domain.Allocation) {
	a := adoptAllocation()
	return &adoptStack{etag: `W/"2026-09-20T12:54:28.018932+00:00"`,
		prefixes: map[int]map[string]any{42: reservedPrefix(42, a, "op-1", 7)}}, a
}

// claimants is the question the whole operation turns on, asked of the stored
// objects rather than of the adapter's answer: which prefixes still carry this
// allocation's marker.
func claimants(stack *adoptStack, allocationID string) []int {
	var out []int
	for id, x := range stack.prefixes {
		fields, _ := x["custom_fields"].(map[string]any)
		if stringCF(fields, allocationIDCF) == allocationID {
			out = append(out, id)
		}
	}
	return out
}

func TestCancelDeletesTheReservationsOwnPrefix(t *testing.T) {
	stack, a := cancelStack()
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "DELETE /api/ipam/prefixes/42/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	// The evidence is read twice before anything is destroyed: the marker
	// search, then the detail read that carries the ETag. The confirmation read
	// after the delete is the third.
	wantRequests := []string{
		"GET /api/ipam/prefixes/", "GET /api/ipam/prefixes/42/",
		"DELETE /api/ipam/prefixes/42/", "GET /api/ipam/prefixes/42/",
	}
	if len(stack.requests) != len(wantRequests) {
		t.Fatalf("requests %v, want %v", stack.requests, wantRequests)
	}
	for i, want := range wantRequests {
		if stack.requests[i] != want {
			t.Fatalf("request %d is %q, want %q", i, stack.requests[i], want)
		}
	}
	if stack.ifMatch[0] != stack.etag {
		t.Fatalf("the delete was not conditional: If-Match %q, ETag %q", stack.ifMatch[0], stack.etag)
	}
	if _, present := stack.prefixes[42]; present {
		t.Fatalf("the prefix is still there: %#v", stack.prefixes[42])
	}
	if left := claimants(stack, a.ID); len(left) != 0 {
		t.Fatalf("prefixes %v still claim allocation %s", left, a.ID)
	}
}

func TestCancelRepeatsWithoutWriting(t *testing.T) {
	stack, a := cancelStack()
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil
	// A cancel interrupted between the delete and the removal of the ledger row
	// re-runs into a prefix that is no longer there; converging on that without
	// writing is what lets the second run finish.
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatalf("a repeated cancel must converge: %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated cancel wrote %v", writes)
	}
}

func TestCancelRefusesEverythingButOneReservationsOwnPrefix(t *testing.T) {
	a := adoptAllocation()
	imported := reservedPrefix(42, a, "op-1", 7)
	imported["tags"] = []any{map[string]any{"slug": ImportedTag}}
	noOperation := reservedPrefix(42, a, "op-1", 7)
	noOperationFields, _ := noOperation["custom_fields"].(map[string]any)
	noOperationFields[operationIDCF] = nil
	cases := map[string]struct {
		prefixes map[int]map[string]any
		want     error
	}{
		// Nothing carries the marker: the reservation never reached the
		// inventory, which is the commonest shape of a stuck hold, or a
		// previous run already deleted it. Both are success.
		"nothing claims the allocation": {map[int]map[string]any{
			43: importedPrefix(43, "10.0.2.0/24", 7),
		}, nil},
		"two prefixes claim the allocation": {map[int]map[string]any{
			42: reservedPrefix(42, a, "op-1", 7),
			43: reservedPrefix(43, a, "op-1", 7),
		}, ErrCancelAmbiguous},
		"the single claimant is another operation's": {map[int]map[string]any{
			42: reservedPrefix(42, a, "op-9", 7),
		}, ErrCancelNotOurs},
		// A prefix carrying the allocation id and no operation id at all is a
		// half-written object of unknown provenance, not this cancel's.
		"the single claimant carries no operation id": {map[int]map[string]any{
			42: noOperation,
		}, ErrCancelNotOurs},
		// Ensure writes no tag, so a reservation's marker on an imported prefix
		// is an out-of-band edit. Deleting it would destroy occupancy the
		// import recorded and this reservation never created (ADR 0013).
		"the single claimant carries the import tag": {map[int]map[string]any{
			42: imported,
		}, ErrCancelImported},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{etag: `W/"1"`, prefixes: tc.prefixes}
			c, closer := adoptClient(t, stack)
			defer closer()
			before := len(stack.prefixes)
			err := c.CancelReservation(context.Background(), adoptAllocation(), "op-1")
			if tc.want == nil && err != nil {
				t.Fatalf("got %v, want success", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("wrote %v", writes)
			}
			if len(stack.prefixes) != before {
				t.Fatalf("the estate lost an object: %d prefixes, was %d", len(stack.prefixes), before)
			}
			// Every one of these is decided by the marker search alone. The
			// detail read exists to catch a prefix that changes hands between
			// the two reads, not to reach a verdict the list already carries,
			// so a refusal here costs exactly one request.
			if len(stack.requests) != 1 || stack.requests[0] != "GET /api/ipam/prefixes/" {
				t.Fatalf("requests %v, want only the marker search", stack.requests)
			}
		})
	}
}

func TestCancelRefusesBeforeContactingNetBox(t *testing.T) {
	for name, mutate := range map[string]func(a *domain.Allocation) string{
		"no allocation ID": func(a *domain.Allocation) string { a.ID = ""; return "op-1" },
		"no operation ID":  func(a *domain.Allocation) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{prefixes: map[int]map[string]any{}}
			c, closer := adoptClient(t, stack)
			defer closer()
			a := adoptAllocation()
			operationID := mutate(&a)
			if err := c.CancelReservation(context.Background(), a, operationID); !errors.Is(err, ErrCancelInvalid) {
				t.Fatalf("got %v, want %v", err, ErrCancelInvalid)
			}
			if len(stack.requests) != 0 {
				t.Fatalf("a refusal contacted NetBox: %v", stack.requests)
			}
		})
	}
}

// NetBox 4.6.7 answers 412 to a DELETE whose If-Match no longer matches, and
// leaves the object in place (measured for H8a; ADR 0013). Whatever moved the
// prefix has to be looked at before anything is destroyed, so this is a refusal
// and never a retry.
func TestCancelRefusesWhenThePrefixChangedUnderTheConditionalDelete(t *testing.T) {
	stack, a := cancelStack()
	stack.deleteStatus = http.StatusPreconditionFailed
	c, closer := adoptClient(t, stack)
	defer closer()
	err := c.CancelReservation(context.Background(), a, "op-1")
	if !errors.Is(err, ErrCancelConflict) {
		t.Fatalf("got %v, want %v", err, ErrCancelConflict)
	}
	if writes := stack.writes(); len(writes) != 1 {
		t.Fatalf("expected one refused DELETE and no retry, got %v", writes)
	}
	if classifiedUncertainByTheWorker(err) || errors.Is(err, domain.ErrInventoryUncertain) {
		t.Fatalf("a 412 must not be classified as uncertain: %v", err)
	}
	if _, present := stack.prefixes[42]; !present {
		t.Fatal("a refused conditional delete still removed the prefix")
	}
}

// The claimant is found by a list read and deleted after a detail read, and
// those are two reads. Whatever lands between them decides: a prefix that has
// become another allocation's, another operation's, or an import's is not ours
// to destroy, and nothing is written.
func TestCancelTrustsTheDetailReadOverTheList(t *testing.T) {
	cases := map[string]struct {
		change func(x map[string]any)
		want   error
	}{
		"another allocation took the prefix": {func(x map[string]any) {
			fields, _ := x["custom_fields"].(map[string]any)
			fields[allocationIDCF] = "alloc-somebody-else"
		}, ErrCancelConflict},
		"another operation rewrote it": {func(x map[string]any) {
			fields, _ := x["custom_fields"].(map[string]any)
			fields[operationIDCF] = "op-somebody-else"
		}, ErrCancelNotOurs},
		"an import claimed it": {func(x map[string]any) {
			x["tags"] = []any{map[string]any{"slug": ImportedTag}}
		}, ErrCancelImported},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack, a := cancelStack()
			stack.beforeRead = tc.change
			c, closer := adoptClient(t, stack)
			defer closer()
			if err := c.CancelReservation(context.Background(), a, "op-1"); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("a refusal wrote %v", writes)
			}
			if _, present := stack.prefixes[42]; !present {
				t.Fatal("a refusal still removed the prefix")
			}
		})
	}
}

// The marker itself can go between the two reads. With every ownership field
// gone nothing claims the allocation any more, which is the question this call
// answers, and what is left is not ours to delete. With only the allocation id
// gone the object is still somebody's hold, so it is a conflict and stays.
func TestCancelDecidesAVanishedMarkerByWhatIsLeft(t *testing.T) {
	cases := map[string]struct {
		change func(x map[string]any)
		want   error
	}{
		"every ownership field was cleared": {func(x map[string]any) {
			x["custom_fields"] = map[string]any{}
		}, nil},
		"only the allocation id was cleared": {func(x map[string]any) {
			fields, _ := x["custom_fields"].(map[string]any)
			fields[allocationIDCF] = ""
		}, ErrCancelConflict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack, a := cancelStack()
			stack.beforeRead = tc.change
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.CancelReservation(context.Background(), a, "op-1")
			if tc.want == nil && err != nil {
				t.Fatalf("nothing claims the allocation, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("a prefix that lost its marker was written to: %v", writes)
			}
			if _, present := stack.prefixes[42]; !present {
				t.Fatal("a prefix that is no longer this allocation's was removed")
			}
		})
	}
}

// The delete answered and the confirmation read did not. Absence is what the
// caller's ledger delete turns on, so an unanswered read-back is uncertain and
// an answered refusal of it is not the service's kind of uncertain -- and
// neither is ever reported as a removal.
func TestCancelDoesNotReportARemovalItCouldNotConfirm(t *testing.T) {
	for status, unanswered := range map[int]bool{http.StatusServiceUnavailable: true, http.StatusForbidden: false} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stack, a := cancelStack()
			stack.afterDelete = func(s *adoptStack) { s.readStatus = status }
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.CancelReservation(context.Background(), a, "op-1")
			if !errors.Is(err, ErrCancelUncertain) {
				t.Fatalf("got %v, want %v", err, ErrCancelUncertain)
			}
			if got := errors.Is(err, domain.ErrInventoryUncertain); got != unanswered {
				t.Fatalf("domain.ErrInventoryUncertain = %v for a %d, want %v", got, status, unanswered)
			}
			if writes := stack.writes(); len(writes) != 1 {
				t.Fatalf("expected one DELETE and no retry, got %v", writes)
			}
		})
	}
}

// The delete answered, and the object is still there. Something else is going
// on with that prefix, so the caller is told rather than asked to write again,
// and the hold stays in the ledger where the marker can still find it.
func TestCancelRefusesAPrefixThatIsStillThereAfterTheDelete(t *testing.T) {
	stack, a := cancelStack()
	stack.afterDelete = func(s *adoptStack) { s.prefixes[42] = reservedPrefix(42, a, "op-1", 7) }
	c, closer := adoptClient(t, stack)
	defer closer()
	err := c.CancelReservation(context.Background(), a, "op-1")
	if !errors.Is(err, ErrCancelReadBack) {
		t.Fatalf("got %v, want %v", err, ErrCancelReadBack)
	}
	if writes := stack.writes(); len(writes) != 1 {
		t.Fatalf("expected one DELETE and no retry, got %v", writes)
	}
	if classifiedUncertainByTheWorker(err) || errors.Is(err, domain.ErrInventoryUncertain) {
		t.Fatalf("a read-back refusal must not be classified as uncertain: %v", err)
	}
}

// Somebody removed the object between the detail read and the delete, so NetBox
// answers the delete 404. That is the outcome this call was asking for, and the
// confirmation read is what decides it rather than the status of the write.
func TestCancelConvergesWhenTheObjectWentAwayBeforeTheDelete(t *testing.T) {
	stack, a := cancelStack()
	stack.beforeRead = func(map[string]any) { delete(stack.prefixes, 42) }
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatalf("a delete of an object already gone must converge: %v", err)
	}
	if left := claimants(stack, a.ID); len(left) != 0 {
		t.Fatalf("prefixes %v still claim allocation %s", left, a.ID)
	}
}

func TestCancelReportsAHungDeleteAsUncertain(t *testing.T) {
	stack, a := cancelStack()
	stack.timeout = 50 * time.Millisecond
	stack.deleteDelay = 500 * time.Millisecond
	c, closer := adoptClient(t, stack)
	defer closer()
	err := c.CancelReservation(context.Background(), a, "op-1")
	if err == nil {
		t.Fatal("a delete that never answered must not be reported as a removal")
	}
	if !errors.Is(err, ErrCancelUncertain) {
		t.Fatalf("got %v, want %v", err, ErrCancelUncertain)
	}
	if !errors.Is(err, domain.ErrInventoryUncertain) || !classifiedUncertainByTheWorker(err) {
		t.Fatalf("the service's own classification calls %v a refusal", err)
	}
	for _, refusal := range []error{ErrCancelConflict, ErrCancelReadBack, ErrCancelNotOurs, ErrCancelAmbiguous, ErrCancelImported, ErrCancelInvalid} {
		if errors.Is(err, refusal) {
			t.Fatalf("an uncertain outcome also reads as %v", refusal)
		}
	}
}

// A NetBox that offers no ETag falls back to the unconditional delete, exactly
// as Adopt and the abandon fall back to an unconditional write; the two reads
// before it are what catch a foreign change there.
func TestCancelDeletesUnconditionallyWithoutAnETag(t *testing.T) {
	a := adoptAllocation()
	stack := &adoptStack{prefixes: map[int]map[string]any{42: reservedPrefix(42, a, "op-1", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	if stack.ifMatch[0] != "" {
		t.Fatalf("If-Match was sent without an ETag to match: %q", stack.ifMatch[0])
	}
	if _, present := stack.prefixes[42]; present {
		t.Fatal("the prefix survived an unconditional delete")
	}
}

// Measured against the development NetBox 4.6.7 for package H8a and recorded in
// ADR 0013: deleting a prefix that contains other prefixes is neither refused
// nor cascaded, because nesting is computed from the CIDR. A stuck VPC
// reservation can contain somebody else's prefix, so what happens to that
// prefix is pinned here: nothing.
func TestCancelLeavesAPrefixNestedInsideTheOneItDeletes(t *testing.T) {
	a := adoptAllocation()
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: reservedPrefix(42, a, "op-1", 7), 43: importedPrefix(43, "10.0.1.128/25", 7),
	}}
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "DELETE /api/ipam/prefixes/42/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	inside, present := stack.prefixes[43]
	if !present {
		t.Fatal("the nested prefix went with the one the cancel deleted")
	}
	if inside["prefix"] != "10.0.1.128/25" {
		t.Fatalf("the nested prefix reads as %#v", inside["prefix"])
	}
	fields, _ := inside["custom_fields"].(map[string]any)
	kept := prefix{CustomFields: fields, Tags: []awsTag{{Slug: ImportedTag}}}
	if !kept.imported() || kept.owned() {
		t.Fatalf("the nested prefix changed: imported=%v owned=%v", kept.imported(), kept.owned())
	}
	if stringCF(fields, ImportBatchField) != "batch-1" {
		t.Fatalf("the nested prefix lost its import batch: %#v", fields)
	}
}

// The round trip this operation exists for: let Ensure create the reservation's
// prefix, cancel it, and show that nothing claims the allocation afterwards
// while occupancy an import wrote elsewhere is exactly as it was.
func TestCancelRemovesThePrefixEnsureCreated(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`,
		prefixes: map[int]map[string]any{43: importedPrefix(43, "10.0.2.0/24", 7)}}
	untouched, _ := stack.prefixes[43]["custom_fields"].(map[string]any)
	wasImported := map[string]any{}
	for key, value := range untouched {
		wasImported[key] = value
	}

	c, closer := adoptClient(t, stack)
	defer closer()
	a := adoptAllocation()
	id, err := c.Ensure(context.Background(), a, "op-1")
	if err != nil {
		t.Fatalf("the fixture could not be reserved: %v", err)
	}
	if left := claimants(stack, a.ID); len(left) != 1 {
		t.Fatalf("Ensure left %v claiming allocation %s, want exactly one", left, a.ID)
	}
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil

	if err := c.CancelReservation(context.Background(), a, "op-1"); err != nil {
		t.Fatalf("the prefix Ensure created could not be cancelled: %v", err)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "DELETE /api/ipam/prefixes/"+id+"/" {
		t.Fatalf("writes %v, want one DELETE of the prefix Ensure created (%s)", writes, id)
	}
	if left := claimants(stack, a.ID); len(left) != 0 {
		t.Fatalf("prefixes %v still claim allocation %s", left, a.ID)
	}
	for _, x := range stack.prefixes {
		if x["prefix"] == a.CIDR {
			t.Fatalf("something still holds %s: %#v", a.CIDR, x)
		}
	}

	// Nothing but the reservation's own object was touched: the import tag and
	// every field an import wrote read back exactly as they were.
	kept, present := stack.prefixes[43]
	if !present {
		t.Fatal("the imported prefix elsewhere in the estate was deleted too")
	}
	now, _ := kept["custom_fields"].(map[string]any)
	for key, was := range wasImported {
		if now[key] != was {
			t.Fatalf("%s is %#v, want %#v -- what the import wrote", key, now[key], was)
		}
	}
	tags, _ := kept["tags"].([]any)
	if tag, _ := tags[0].(map[string]any); len(tags) != 1 || tag["slug"] != ImportedTag {
		t.Fatalf("the import tag did not survive: %#v", kept["tags"])
	}
}
