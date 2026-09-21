package netbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// adoptedPrefix is what a half-finished adoption leaves behind: the object an
// import created, with this allocation's ownership fields written over it and
// its status changed, and with the import tag, batch, source and the operator's
// own field still there. It is built by running the real Adopt against the
// shared fake rather than by hand, so these tests cannot drift from what an
// adoption actually writes.
func adoptedPrefix(t *testing.T, stack *adoptStack, a domain.Allocation, operationID string) {
	t.Helper()
	c, closer := adoptClient(t, stack)
	defer closer()
	if _, err := c.Adopt(context.Background(), a, operationID); err != nil {
		t.Fatalf("the fixture could not be adopted: %v", err)
	}
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil
}

func abandonStack(t *testing.T) (*adoptStack, domain.Allocation) {
	t.Helper()
	stack := &adoptStack{etag: `W/"2026-09-20T07:40:05.518754+00:00"`,
		prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	a := adoptAllocation()
	adoptedPrefix(t, stack, a, "op-1")
	return stack, a
}

// bodyFields is the custom_fields map of the n-th write the fake recorded.
func bodyFields(t *testing.T, stack *adoptStack, n int) map[string]any {
	t.Helper()
	if len(stack.bodies) <= n {
		t.Fatalf("expected at least %d writes, got %d", n+1, len(stack.bodies))
	}
	fields, ok := stack.bodies[n]["custom_fields"].(map[string]any)
	if !ok {
		t.Fatalf("write %d carries no custom_fields: %#v", n, stack.bodies[n])
	}
	return fields
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAbandonClearsTheHalfConvertedPrefix(t *testing.T) {
	stack, a := abandonStack(t)
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{Status: "reserved"}); err != nil {
		t.Fatal(err)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "PATCH /api/ipam/prefixes/42/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	if stack.ifMatch[0] != stack.etag {
		t.Fatalf("the clear was not conditional: If-Match %q, ETag %q", stack.ifMatch[0], stack.etag)
	}
	body := stack.bodies[0]
	if body["status"] != "reserved" {
		t.Fatalf("status is %#v, want the recorded prior status", body["status"])
	}
	// A tags list in the body would replace the prefix's tags and take the
	// import provenance with it -- the thing that makes the cleared prefix
	// occupancy again rather than an orphan.
	if _, present := body["tags"]; present {
		t.Fatalf("the clear carries a tags list: %#v", body["tags"])
	}
	fields := bodyFields(t, stack, 0)
	for _, key := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		if value, present := fields[key]; !present || value != nil {
			t.Fatalf("owned field %s is %#v, want an explicit null", key, value)
		}
	}
	// Custom fields merge key by key, so a key the body does not carry is left
	// exactly as it is. Sending one would overwrite an operator's own value.
	for _, key := range []string{ImportBatchField, ImportSourceField, "operator_note"} {
		if _, present := fields[key]; present {
			t.Fatalf("the clear sends the unowned field %s", key)
		}
	}

	after := stack.prefixes[42]
	if after["status"] != "reserved" {
		t.Fatalf("the prefix status is %#v", after["status"])
	}
	tags, _ := after["tags"].([]any)
	if tag, _ := tags[0].(map[string]any); len(tags) != 1 || tag["slug"] != ImportedTag {
		t.Fatalf("the import tag did not survive: %#v", after["tags"])
	}
	stored, _ := after["custom_fields"].(map[string]any)
	for key, want := range map[string]string{
		ImportBatchField: "batch-1", ImportSourceField: "networks.csv", "operator_note": "keep me",
	} {
		if stored[key] != want {
			t.Fatalf("unowned field %s is %#v, want %q", key, stored[key], want)
		}
	}
	// What every later reader asks of the object: it owns nothing and it is
	// still occupancy this platform imported.
	cleared := prefix{CustomFields: stored, Tags: []awsTag{{Slug: ImportedTag}}}
	if cleared.owned() {
		t.Fatalf("the prefix still reads as owned: %#v", stored)
	}
}

func TestAbandonRepeatsWithoutWriting(t *testing.T) {
	stack, a := abandonStack(t)
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); err != nil {
		t.Fatal(err)
	}
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil
	// An abandon interrupted between the clear and the delete re-runs into a
	// prefix that no longer carries the marker; converging on it without
	// writing is what lets the second run finish.
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); err != nil {
		t.Fatalf("a repeated abandon must converge: %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated abandon wrote %v", writes)
	}
}

func TestAbandonRefusesEverythingButOneHalfConvertedPrefix(t *testing.T) {
	cases := map[string]struct {
		prefixes map[int]map[string]any
		want     error
	}{
		// Nothing carries the marker: the adoption never reached the inventory,
		// or a previous run already cleared it. Both are success.
		"nothing claims the allocation": {map[int]map[string]any{
			42: importedPrefix(42, "10.0.1.0/24", 7),
		}, nil},
		"two prefixes claim the allocation": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{allocationIDCF: "alloc-1", operationIDCF: "op-1"}),
			43: ownedBy(43, "10.0.2.0/24", map[string]any{allocationIDCF: "alloc-1", operationIDCF: "op-1"}),
		}, ErrAbandonAmbiguous},
		"the single claimant is another operation's": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{allocationIDCF: "alloc-1", operationIDCF: "op-9"}),
		}, ErrAbandonNotOurs},
		// A prefix carrying the allocation id and no operation id at all is a
		// half-written object of unknown provenance, not this abandon's.
		"the single claimant carries no operation id": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{allocationIDCF: "alloc-1"}),
		}, ErrAbandonNotOurs},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{etag: `W/"1"`, prefixes: tc.prefixes}
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.AbandonAdoption(context.Background(), adoptAllocation(), "op-1", domain.PriorInventory{})
			if tc.want == nil && err != nil {
				t.Fatalf("got %v, want success", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("wrote %v", writes)
			}
		})
	}
}

func TestAbandonRefusesBeforeContactingNetBox(t *testing.T) {
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
			if err := c.AbandonAdoption(context.Background(), a, operationID, domain.PriorInventory{}); !errors.Is(err, ErrAbandonInvalid) {
				t.Fatalf("got %v, want %v", err, ErrAbandonInvalid)
			}
			if len(stack.requests) != 0 {
				t.Fatalf("a refusal contacted NetBox: %v", stack.requests)
			}
		})
	}
}

func TestAbandonRefusesWhenThePrefixChangedUnderTheConditionalWrite(t *testing.T) {
	stack, a := abandonStack(t)
	stack.patchStatus = http.StatusPreconditionFailed
	c, closer := adoptClient(t, stack)
	defer closer()
	err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{})
	if !errors.Is(err, ErrAbandonConflict) {
		t.Fatalf("got %v, want %v", err, ErrAbandonConflict)
	}
	if writes := stack.writes(); len(writes) != 1 {
		t.Fatalf("expected one refused PATCH and no retry, got %v", writes)
	}
	// A 412 is a refusal, never a retry: whatever changed the prefix has to be
	// looked at before anything else is cleared or deleted.
	if classifiedUncertainByTheWorker(err) {
		t.Fatalf("a 412 must not be classified as uncertain")
	}
	fields, _ := stack.prefixes[42]["custom_fields"].(map[string]any)
	if fields[allocationIDCF] != a.ID {
		t.Fatalf("a refused conditional write still changed the prefix: %#v", fields)
	}
}

func TestAbandonRefusesAReadBackThatIsNotUnownedOccupancy(t *testing.T) {
	for name, corrupt := range map[string]func(x map[string]any){
		"an ownership field is back": func(x map[string]any) {
			fields, _ := x["custom_fields"].(map[string]any)
			fields[allocationIDCF] = "alloc-1"
		},
		"the import tag is gone": func(x map[string]any) { x["tags"] = []any{} },
	} {
		t.Run(name, func(t *testing.T) {
			stack, a := abandonStack(t)
			stack.afterPatch = corrupt
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{})
			if !errors.Is(err, ErrAbandonReadBack) {
				t.Fatalf("got %v, want %v", err, ErrAbandonReadBack)
			}
			// The write has already happened. A second one would overwrite
			// whoever holds the prefix now, so the caller stops instead.
			if writes := stack.writes(); len(writes) != 1 {
				t.Fatalf("expected one PATCH and no retry, got %v", writes)
			}
			if classifiedUncertainByTheWorker(err) {
				t.Fatalf("a read-back refusal must not be classified as uncertain")
			}
		})
	}
}

// classifiedUncertainByTheWorker repeats internal/service's
// uncertainInventoryError exactly. H2b decides between stopping the abandon and
// refusing it on that answer, so the two have to agree about which of this
// adapter's errors say nothing about the prefix.
func classifiedUncertainByTheWorker(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func TestAbandonReportsAHungWriteAsUncertain(t *testing.T) {
	stack, a := abandonStack(t)
	stack.timeout = 50 * time.Millisecond
	stack.patchDelay = 500 * time.Millisecond
	c, closer := adoptClient(t, stack)
	defer closer()
	err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{})
	if err == nil {
		t.Fatal("a write that never answered must not be reported as a clear")
	}
	if !errors.Is(err, ErrAbandonUncertain) {
		t.Fatalf("got %v, want %v", err, ErrAbandonUncertain)
	}
	if !classifiedUncertainByTheWorker(err) {
		t.Fatalf("the worker's own classification calls %v a refusal", err)
	}
	for _, refusal := range []error{ErrAbandonConflict, ErrAbandonReadBack, ErrAbandonNotOurs, ErrAbandonAmbiguous, ErrAbandonInvalid} {
		if errors.Is(err, refusal) {
			t.Fatalf("an uncertain outcome also reads as %v", refusal)
		}
	}
}

// A NetBox that offers no ETag falls back to the unconditional write, exactly
// as Adopt does; the re-read is what catches a foreign change there.
func TestAbandonWritesUnconditionallyWithoutAnETag(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	a := adoptAllocation()
	adoptedPrefix(t, stack, a, "op-1")
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); err != nil {
		t.Fatal(err)
	}
	if stack.ifMatch[0] != "" {
		t.Fatalf("If-Match was sent without an ETag to match: %q", stack.ifMatch[0])
	}
}

func TestAbandonRestoresTheStatusTheAdoptionOverwrote(t *testing.T) {
	for name, tc := range map[string]struct{ prior, want string }{
		"a recorded prior status":            {"deprecated", "deprecated"},
		"a hold that recorded none":          {"", importedStatus},
		"a status this NetBox would refuse":  {"whatever-an-operator-typed", importedStatus},
		"the status an import itself writes": {"active", "active"},
	} {
		t.Run(name, func(t *testing.T) {
			stack, a := abandonStack(t)
			c, closer := adoptClient(t, stack)
			defer closer()
			if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{Status: tc.prior}); err != nil {
				t.Fatal(err)
			}
			if got := stack.bodies[0]["status"]; got != tc.want {
				t.Fatalf("status is %#v, want %q", got, tc.want)
			}
		})
	}
}

// everyOwnedField is the set of keys an adoption writes, which is what the
// clear empties. It is spelled out here rather than derived from the
// implementation so that a key added to ownedFields fails this test and has to
// be argued about, instead of being emptied silently.
var everyOwnedField = []string{
	"platform_allocation_id", "platform_allocation_key", "platform_operation_id",
	"platform_parent_allocation_id", "platform_tenant_id", "platform_environment",
	"platform_pool_id", "platform_policy_version", "platform_state",
	"platform_aws_account_id", "platform_aws_region", "platform_aws_az_id",
	// Written only when the allocation carries the value. An uncommitted
	// adoption -- the only kind that can be abandoned -- carries none of them.
	"platform_aws_resource_id", "platform_last_observed_at", "platform_quarantine_until",
}

func TestTheClearEmptiesExactlyWhatAnAdoptionWrites(t *testing.T) {
	now := time.Now().UTC()
	full := adoptAllocation()
	full.Binding = &domain.Binding{ResourceID: "vpc-0abc"}
	full.LastObservedAt = &now
	full.QuarantineUntil = &now

	got := keys(ownedFields(full, "op-1"))
	want := append([]string(nil), everyOwnedField...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ownedFields writes %d keys, this test knows %d:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ownedFields writes %v, this test knows %v", got, want)
		}
	}

	// Both shapes of allocation, so the conditional keys are covered in each
	// direction: the clear empties every key that shape's adoption writes and
	// not one key more.
	for name, a := range map[string]domain.Allocation{
		"an uncommitted hold": adoptAllocation(),
		"every owned field":   full,
	} {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{etag: `W/"1"`,
				prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
			adoptedPrefix(t, stack, a, "op-1")
			c, closer := adoptClient(t, stack)
			defer closer()
			if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); err != nil {
				t.Fatal(err)
			}
			cleared := keys(bodyFields(t, stack, 0))
			expected := keys(ownedFields(a, "op-1"))
			if len(cleared) != len(expected) {
				t.Fatalf("the clear sends %v, an adoption writes %v", cleared, expected)
			}
			for i := range cleared {
				if cleared[i] != expected[i] {
					t.Fatalf("the clear sends %v, an adoption writes %v", cleared, expected)
				}
			}
			for _, key := range cleared {
				if value := bodyFields(t, stack, 0)[key]; value != nil {
					t.Fatalf("the clear sends %s as %#v, want an explicit null", key, value)
				}
			}
		})
	}
}

// The round trip this operation exists for: adopt the imported prefix, abandon
// the adoption, and compare the stored object with the one the import left.
func TestAbandonReturnsTheObjectAnImportLeft(t *testing.T) {
	before := importedPrefix(42, "10.0.1.0/24", 7)
	stored, _ := before["custom_fields"].(map[string]any)
	wasImported := map[string]any{}
	for key, value := range stored {
		wasImported[key] = value
	}

	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	a := adoptAllocation()
	adoptedPrefix(t, stack, a, "op-1")
	c, closer := adoptClient(t, stack)
	defer closer()
	// What importedPrefix carries, and so what the service will have recorded
	// from the snapshot before the adoption overwrote it.
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{Status: "active", AccountID: "123456789012", Region: "eu-central-1"}); err != nil {
		t.Fatal(err)
	}

	after := stack.prefixes[42]
	if after["prefix"] != before["prefix"] || after["status"] != importedStatus {
		t.Fatalf("prefix %#v status %#v", after["prefix"], after["status"])
	}
	tags, _ := after["tags"].([]any)
	if tag, _ := tags[0].(map[string]any); len(tags) != 1 || tag["slug"] != ImportedTag {
		t.Fatalf("tags %#v", after["tags"])
	}
	now, _ := after["custom_fields"].(map[string]any)
	// Every field the import wrote reads back as the import wrote it -- including
	// the AWS account and region, which an adoption owns and overwrites with
	// the allocation's: the ledger records what they held (AdoptionRecord's
	// PriorAccountID and PriorRegion, read from the snapshot) and the clear
	// puts them back. platform_aws_resource_id is not owned for an unbound
	// allocation, so it survives untouched.
	for key, was := range wasImported {
		if now[key] != was {
			t.Fatalf("%s is %#v, want %#v -- what the import wrote", key, now[key], was)
		}
	}
	// Keys the import never had are present only as nulls, which stringCF and
	// prefix.owned read exactly as an absent key.
	for key, value := range now {
		if _, had := wasImported[key]; !had && value != nil {
			t.Fatalf("the abandon left %s = %#v behind", key, value)
		}
	}
	unowned := prefix{CustomFields: now, Tags: []awsTag{{Slug: ImportedTag}}}
	if unowned.owned() || !unowned.imported() {
		t.Fatalf("owned=%v imported=%v after the round trip", unowned.owned(), unowned.imported())
	}
	// Every key this allocation's adoption wrote reads as unset, except the two
	// the import had written first and the clear restored -- the set the clear
	// derives from ownedFields, not the larger set a bound allocation would
	// have, whose platform_aws_resource_id the adoption never touched.
	restored := map[string]bool{awsAccountCF: true, awsRegionCF: true}
	for _, key := range keys(ownedFields(a, "op-1")) {
		if value := stringCF(now, key); value != "" && !restored[key] {
			t.Fatalf("%s still reads as %q", key, value)
		}
	}
	if stringCF(now, awsResourceCF) != "vpc-0abc" {
		t.Fatalf("%s is %#v, want the value the import wrote: an unbound allocation's adoption never owned it",
			awsResourceCF, now[awsResourceCF])
	}
}

// The claimant is found by a list read and cleared after a detail read, and
// those are two reads. Whatever lands between them decides: a prefix that has
// become another allocation's, or another operation's, is not ours to clear,
// and nothing is written.
func TestAbandonTrustsTheDetailReadOverTheList(t *testing.T) {
	cases := map[string]struct {
		change func(fields map[string]any)
		want   error
	}{
		"another allocation took the prefix": {func(f map[string]any) { f[allocationIDCF] = "alloc-somebody-else" }, ErrAbandonConflict},
		"another operation rewrote it":       {func(f map[string]any) { f[operationIDCF] = "op-somebody-else" }, ErrAbandonNotOurs},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack, a := abandonStack(t)
			stack.beforeRead = func(x map[string]any) {
				fields, _ := x["custom_fields"].(map[string]any)
				tc.change(fields)
			}
			c, closer := adoptClient(t, stack)
			defer closer()
			stack.requests, stack.bodies = nil, nil
			if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("a refusal wrote %v", writes)
			}
		})
	}
}

// Without a recorded account and region -- a hold created before the ledger
// carried them -- the clear empties the two keys rather than leaving the
// allocation's values on a prefix the allocation no longer owns.
func TestAbandonEmptiesTheAccountAndRegionItCannotRestore(t *testing.T) {
	stack, a := abandonStack(t)
	c, closer := adoptClient(t, stack)
	defer closer()
	if err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{}); err != nil {
		t.Fatal(err)
	}
	now, _ := stack.prefixes[42]["custom_fields"].(map[string]any)
	if now[awsAccountCF] != nil || now[awsRegionCF] != nil {
		t.Fatalf("account %#v region %#v, want both null", now[awsAccountCF], now[awsRegionCF])
	}
}
