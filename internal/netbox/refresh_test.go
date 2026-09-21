package netbox

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Package M9b3 (ADR 0016): onboard apply --refresh, the first update path
// EnsureOccupancy has ever had. These tests reuse adoptStack
// (internal/netbox/adopt_test.go) rather than a second fake: it already
// answers a conditional detail read with an ETag and merges a PATCH's
// custom_fields key by key, exactly like NetBox 4.6.7 (measured for package
// F1, ADR 0010) -- precisely the surface RefreshOccupancy needs.

func obs(t string) *string { return &t }

func refreshContributor(resource, cidr, observedAt string) domain.Contributor {
	return domain.Contributor{
		Identity:  "aws:000000000001:eu-central-1:" + resource + ":" + cidr,
		AccountID: "000000000001", Region: "eu-central-1", Type: "vpc", ResourceID: resource,
		ObservedAt: obs(observedAt), FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
		SourceFile: "networks.csv", SourceRow: 2,
	}
}

// degenerateContributor is the identity string ADR 0016 and the M9b1 review
// note call degenerate: no resource_id column, so several different rows can
// produce the exact same string.
func degenerateContributor(cidr string, sourceRow int) domain.Contributor {
	return domain.Contributor{
		Identity:  "aws:000000000001:eu-central-1::" + cidr,
		AccountID: "000000000001", Region: "eu-central-1", Type: "vpc",
		FirstSeenBatch: "batch-2", LastSeenBatch: "batch-2",
		SourceFile: "networks.csv", SourceRow: sourceRow,
	}
}

func refreshPrefixWithContributors(id int, cidr string, contributors []domain.Contributor) map[string]any {
	x := importedPrefix(id, cidr, 7)
	if len(contributors) == 0 {
		return x
	}
	return withContributors(x, contributors...)
}

// testDomainOnly is testDomain's Domain half, named for a call site that
// never needs the pool.
func testDomainOnly(t *testing.T) domain.Domain {
	t.Helper()
	d, _ := testDomain()
	return d
}

func TestRefreshOccupancyAddsANewContributorWithExactlyOnePATCH(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: refreshPrefixWithContributors(42, "10.0.1.0/24", existing)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	table := []domain.Contributor{
		refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z"),
		refreshContributor("vpc-b", "10.0.1.0/24", "2026-09-22T11:00:00Z"),
	}
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshWritten || result.Reconstructed {
		t.Fatalf("unexpected result %#v", result)
	}
	if writes := stack.writes(); len(writes) != 1 || writes[0] != "PATCH /api/ipam/prefixes/42/" {
		t.Fatalf("expected exactly one PATCH, got %v", writes)
	}
	if stack.ifMatch[0] != stack.etag {
		t.Fatalf("write was not conditional: If-Match %q, ETag %q", stack.ifMatch[0], stack.etag)
	}
	body := stack.bodies[0]
	fields, _ := body["custom_fields"].(map[string]any)
	if fields[ImportSourceField] != "networks.csv" {
		t.Fatalf("platform_import_source was not written: %#v", fields)
	}
	if _, present := body["description"]; present {
		t.Fatal("a refresh must never touch the description")
	}
	if _, present := body["status"]; present {
		t.Fatal("a refresh must never touch the status")
	}
	if _, present := body["tags"]; present {
		t.Fatal("a refresh must never touch the tags")
	}
	got := contributorsOn(t, stack.prefixes[42])
	if len(got) != 2 {
		t.Fatalf("expected two contributors, got %#v", got)
	}
	by := map[string]domain.Contributor{}
	for _, c := range got {
		by[c.ResourceID] = c
	}
	// vpc-a was named again by this table (it is in `table` above, not only
	// in `existing`), and the write happened because vpc-b is new -- ADR
	// 0016's merge rule refreshes every identity the table names on such a
	// write, not only the one that caused it. refreshContributor gives both
	// the existing and the table copy the same "batch-1", so this only
	// proves the value landed at "batch-1"; the case where the two differ
	// is TestRefreshOccupancyRefreshesObservedAtAndLastSeenBatchKeepingFirstSeenBatch.
	if by["vpc-a"].FirstSeenBatch != "batch-1" || by["vpc-a"].LastSeenBatch != "batch-1" {
		t.Fatalf("vpc-a's batches: %#v", by["vpc-a"])
	}
	if by["vpc-b"].FirstSeenBatch != "batch-1" || by["vpc-b"].LastSeenBatch != "batch-1" {
		t.Fatalf("the new entry's batches: %#v", by["vpc-b"])
	}
}

func TestRefreshOccupancyRefreshesObservedAtAndLastSeenBatchKeepingFirstSeenBatch(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: refreshPrefixWithContributors(42, "10.0.1.0/24", existing)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	// The table names vpc-a again (newer observed_at, a later batch) AND a
	// genuinely new vpc-b, so the set grows and a write happens; the
	// existing entry's own fields other than observed_at/last_seen_batch
	// must not move.
	table := []domain.Contributor{
		{Identity: existing[0].Identity, AccountID: "000000000001", Region: "eu-central-1", Type: "vpc",
			ResourceID: "vpc-a", ObservedAt: obs("2026-09-22T12:00:00Z"),
			FirstSeenBatch: "batch-2", LastSeenBatch: "batch-2", SourceFile: "networks.csv", SourceRow: 2},
		refreshContributor("vpc-b", "10.0.1.0/24", "2026-09-22T11:00:00Z"),
	}
	if _, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table); err != nil {
		t.Fatal(err)
	}
	got := contributorsOn(t, stack.prefixes[42])
	by := map[string]domain.Contributor{}
	for _, c := range got {
		by[c.ResourceID] = c
	}
	a := by["vpc-a"]
	if *a.ObservedAt != "2026-09-22T12:00:00Z" {
		t.Fatalf("observed_at must be refreshed from the table: %#v", a)
	}
	if a.LastSeenBatch != "batch-2" {
		t.Fatalf("last_seen_batch must be refreshed from the table: %#v", a)
	}
	if a.FirstSeenBatch != "batch-1" {
		t.Fatalf("first_seen_batch must be kept from the existing entry: %#v", a)
	}
}

func TestRefreshOccupancyMakesNoWriteWhenTheSetDoesNotChange(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: refreshPrefixWithContributors(42, "10.0.1.0/24", existing)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	// Same identity, a strictly newer observed_at and a new batch name --
	// ADR 0014's amendment, applied to the structured field: a write happens
	// only when the SET changes, never because a timestamp moved.
	table := []domain.Contributor{
		{Identity: existing[0].Identity, AccountID: "000000000001", Region: "eu-central-1", Type: "vpc",
			ResourceID: "vpc-a", ObservedAt: obs("2026-09-23T00:00:00Z"),
			FirstSeenBatch: "batch-9", LastSeenBatch: "batch-9", SourceFile: "networks.csv", SourceRow: 2},
	}
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshUnchanged {
		t.Fatalf("unexpected result %#v", result)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("an unchanged set must write nothing, got %v", writes)
	}
	got := contributorsOn(t, stack.prefixes[42])
	if !reflect.DeepEqual(got, existing) {
		t.Fatalf("the stored list moved without a write: %#v", got)
	}
}

// TestRefreshOccupancyReconstructWithNothingToReconstructWritesNothing is
// the defensive mirror of EnsureOccupancy's own "never write an empty
// list" rule (ADR 0016: "an empty list is not an argument that nothing
// contributes; it is a list nobody wrote"), for the reconstruct path.
// Unreachable through onboard apply --refresh in practice -- Plan raises
// RuleAlreadyUnmanaged only for a CIDR the table's own rows produced, so
// contributors is never empty when RefreshOccupancy is called for it -- but
// RefreshOccupancy is a public method any caller can call directly.
func TestRefreshOccupancyReconstructWithNothingToReconstructWritesNothing(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshUnchanged || result.Reconstructed {
		t.Fatalf("unexpected result %#v", result)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a reconstruction with nothing to reconstruct must write nothing, got %v", writes)
	}
}

func TestRefreshOccupancyReconstructsAnAbsentListAndFlagsIt(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshWritten || !result.Reconstructed {
		t.Fatalf("unexpected result %#v", result)
	}
	body := stack.bodies[0]
	fields, _ := body["custom_fields"].(map[string]any)
	if fields[ImportContributorsReconstructedField] != true {
		t.Fatalf("reconstructed flag was not sent: %#v", fields)
	}
	stored, _ := stack.prefixes[42]["custom_fields"].(map[string]any)
	if stored[ImportContributorsReconstructedField] != true {
		t.Fatalf("reconstructed flag was not stored: %#v", stored)
	}
	got := contributorsOn(t, stack.prefixes[42])
	if !reflect.DeepEqual(got, table) {
		t.Fatalf("reconstructed list = %#v, want %#v", got, table)
	}
}

func TestRefreshOccupancyDoesNotReflagAnAlreadyReconstructedPrefix(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	x := refreshPrefixWithContributors(42, "10.0.1.0/24", existing)
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsReconstructedField] = true
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: x}}
	c, closer := adoptClient(t, stack)
	defer closer()

	table := []domain.Contributor{
		refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z"),
		refreshContributor("vpc-b", "10.0.1.0/24", "2026-09-22T11:00:00Z"),
	}
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconstructed {
		t.Fatal("a prefix already flagged reconstructed must not be reported reconstructed again")
	}
	body := stack.bodies[0]
	fields, _ = body["custom_fields"].(map[string]any)
	if _, present := fields[ImportContributorsReconstructedField]; present {
		t.Fatalf("an already-true flag must not be resent: %#v", fields)
	}
}

func TestRefreshOccupancyDegenerateIdentitiesNeverMultiply(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()

	// Three rows with no resource_id column all collapse to one identity
	// string. A refresh must store exactly one entry for it, never three.
	table := []domain.Contributor{
		degenerateContributor("10.0.1.0/24", 2),
		degenerateContributor("10.0.1.0/24", 3),
		degenerateContributor("10.0.1.0/24", 4),
	}
	result, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshWritten {
		t.Fatalf("unexpected result %#v", result)
	}
	got := contributorsOn(t, stack.prefixes[42])
	if len(got) != 1 {
		t.Fatalf("a degenerate identity must never multiply: %#v", got)
	}

	// A second refresh naming the same degenerate identity from a fourth row
	// must not add a second entry either, and -- since the SET (one
	// identity) did not grow -- must write nothing at all.
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil
	second := []domain.Contributor{degenerateContributor("10.0.1.0/24", 9)}
	result, err = c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != RefreshUnchanged {
		t.Fatalf("a degenerate identity already present must not grow the set: %#v", result)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("expected no write, got %v", writes)
	}
	got = contributorsOn(t, stack.prefixes[42])
	if len(got) != 1 {
		t.Fatalf("still expected exactly one entry: %#v", got)
	}
}

func TestRefreshOccupancyRefusesAnyOwnershipFieldNotJustTheImportTag(t *testing.T) {
	owned := ownedBy(42, "10.0.1.0/24", map[string]any{"platform_state": domain.Reserved})
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: owned}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("got %v, want %v", err, ErrOccupancyManaged)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
}

func TestRefreshOccupancyRefusesAnUnreadableList(t *testing.T) {
	x := importedPrefix(42, "10.0.1.0/24", 7)
	fields, _ := x["custom_fields"].(map[string]any)
	fields[ImportContributorsField] = "an operator wrote this"
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: x}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrRefreshUnreadable) {
		t.Fatalf("got %v, want %v", err, ErrRefreshUnreadable)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
}

func TestRefreshOccupancyRefusesA412WithoutRetrying(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, patchStatus: http.StatusPreconditionFailed,
		prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrOccupancyConflict) {
		t.Fatalf("got %v, want %v", err, ErrOccupancyConflict)
	}
	if writes := stack.writes(); len(writes) != 1 {
		t.Fatalf("expected one refused PATCH and no retry, got %v", writes)
	}
}

func TestRefreshOccupancyRefusesWhenNoPrefixHoldsTheCIDR(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrRefreshNotFound) {
		t.Fatalf("got %v, want %v", err, ErrRefreshNotFound)
	}
}

func TestRefreshOccupancyRefusesAContributorWithoutAnIdentity(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{
		42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	bad := refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")
	bad.Identity = ""
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", []domain.Contributor{bad})
	if !errors.Is(err, ErrOccupancyInvalid) {
		t.Fatalf("got %v, want %v", err, ErrOccupancyInvalid)
	}
	if len(stack.requests) != 0 {
		t.Fatalf("a refusal contacted NetBox: %v", stack.requests)
	}
}

// TestRefreshOccupancyNeverTouchesAnAdoptedPrefix guards the pleasant result
// ADR 0016 names: a table row for an adopted network never even reaches
// Plan's RuleAlreadyUnmanaged (overlapsManaged refuses it as an error
// first), but RefreshOccupancy is still tested directly against an adopted
// prefix as a second, independent guard -- exactly the way ADR 0012 is the
// precedent for testing an unowned field's survival rather than assuming it.
func TestRefreshOccupancyNeverTouchesAnAdoptedPrefix(t *testing.T) {
	adopted := ownedBy(42, "10.0.1.0/24", map[string]any{
		allocationIDCF: "alloc-1", allocationKeyCF: "orders", operationIDCF: "op-1", stateCF: domain.Reserved,
	})
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: adopted}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("got %v, want %v", err, ErrOccupancyManaged)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
	tags, _ := stack.prefixes[42]["tags"].([]any)
	if len(tags) == 0 {
		t.Fatal("fixture sanity: an adopted prefix must still carry the import tag")
	}
}

func TestRefreshOccupancyRefusesAPrefixWithNoImportTag(t *testing.T) {
	untagged := importedPrefix(42, "10.0.1.0/24", 7)
	untagged["tags"] = []any{}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: untagged}}
	c, closer := adoptClient(t, stack)
	defer closer()
	table := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	_, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table)
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("got %v, want %v", err, ErrOccupancyManaged)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
}

func TestMergeContributorsNeverWritesOnObservedAtAlone(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	newer := domain.Contributor{Identity: existing[0].Identity, AccountID: "000000000001", Region: "eu-central-1",
		Type: "vpc", ResourceID: "vpc-a", ObservedAt: obs("2099-01-01T00:00:00Z"),
		FirstSeenBatch: "batch-9", LastSeenBatch: "batch-9", SourceFile: "networks.csv", SourceRow: 2}
	if _, changed := mergeContributors(existing, []domain.Contributor{newer}); changed {
		t.Fatal("a newer observed_at on an already-present identity must not, by itself, be a write")
	}
}

func TestMergeContributorsReconstructsAnAbsentList(t *testing.T) {
	want := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	merged, changed := mergeContributors(nil, want)
	if !changed {
		t.Fatal("no list at all must always be a write")
	}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("got %#v, want %#v", merged, want)
	}
}

func TestMergeContributorsNeverDeletesAnEntryTheTableDoesNotName(t *testing.T) {
	existing := []domain.Contributor{
		refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z"),
		refreshContributor("vpc-b", "10.0.1.0/24", "2026-09-22T11:00:00Z"),
	}
	want := []domain.Contributor{
		refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z"),
		refreshContributor("vpc-c", "10.0.1.0/24", "2026-09-22T12:00:00Z"),
	}
	merged, changed := mergeContributors(existing, want)
	if !changed {
		t.Fatal("vpc-c is new; the set grew")
	}
	ids := map[string]bool{}
	for _, c := range merged {
		ids[c.ResourceID] = true
	}
	for _, want := range []string{"vpc-a", "vpc-b", "vpc-c"} {
		if !ids[want] {
			t.Fatalf("%s is missing from the merge; a table naming fewer contributors must never delete: %#v", want, merged)
		}
	}
}

// A table without an observation time never erases one an earlier import
// recorded: a null is no observation.
func TestRefreshOccupancyKeepsARecordedObservationTimeAgainstANullOne(t *testing.T) {
	existing := []domain.Contributor{refreshContributor("vpc-a", "10.0.1.0/24", "2026-09-22T10:00:00Z")}
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{
		42: refreshPrefixWithContributors(42, "10.0.1.0/24", existing)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	old := existing[0]
	old.ObservedAt = nil
	old.LastSeenBatch = "batch-2"
	table := []domain.Contributor{old, refreshContributor("vpc-b", "10.0.1.0/24", "2026-09-22T11:00:00Z")}
	if _, err := c.RefreshOccupancy(context.Background(), testDomainOnly(t), "10.0.1.0/24", "networks.csv", table); err != nil {
		t.Fatal(err)
	}
	for _, got := range contributorsOn(t, stack.prefixes[42]) {
		if got.ResourceID != "vpc-a" {
			continue
		}
		if got.ObservedAt == nil || *got.ObservedAt != "2026-09-22T10:00:00Z" {
			t.Fatalf("a null observation time erased a recorded one: %#v", got)
		}
		if got.LastSeenBatch != "batch-2" {
			t.Fatalf("last_seen_batch must still move: %#v", got)
		}
		return
	}
	t.Fatal("vpc-a is missing from the merged list")
}
