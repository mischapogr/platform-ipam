package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Package M9b1 (ADR 0016): an imported prefix records the cloud resources that
// contribute to it, in an unowned JSON custom field written on CREATE only.
// These tests cover the three properties the record turns on -- what a create
// writes, that an existing prefix is never written, and that the field is
// untouched by adoption, abandonment and the worker's sync -- plus what an
// installation whose NetBox has no such field does.

func testContributor(resource string) domain.Contributor {
	association := "vpc-cidr-assoc-" + resource
	observed := "2026-09-22T10:00:00Z"
	return domain.Contributor{
		Identity:  "aws:000000000001:eu-central-1:" + resource + ":10.1.0.0/16:" + association,
		AccountID: "000000000001", Region: "eu-central-1", Type: "vpc",
		ResourceID: resource, AssociationID: &association, ObservedAt: &observed,
		FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
		SourceFile: "networks.csv", SourceRow: 1,
	}
}

// contributorsOn reads the field back off a stored fixture the way a test
// asserts on it: through JSON, so a comparison is against the shape NetBox
// would really return rather than against Go values a test put there.
func contributorsOn(t *testing.T, x map[string]any) []domain.Contributor {
	t.Helper()
	fields, _ := x["custom_fields"].(map[string]any)
	got, unreadable := contributorsCF(fields, ImportContributorsField)
	if unreadable {
		t.Fatalf("stored contributor list did not decode: %#v", fields[ImportContributorsField])
	}
	return got
}

func TestEnsureOccupancyWritesContributorsOnCreate(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{}, created: map[string]any{
		"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
		"custom_fields": map[string]any{ImportBatchField: "batch-1"},
	}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()

	want := []domain.Contributor{testContributor("vpc-0aaa"), testContributor("vpc-0bbb")}
	got, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Batch: "batch-1", Source: "networks.csv", Contributors: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != OccupancyCreated {
		t.Fatalf("unexpected result %#v", got)
	}

	fields, _ := stack.bodies[0]["custom_fields"].(map[string]any)
	raw, err := json.Marshal(fields[ImportContributorsField])
	if err != nil {
		t.Fatal(err)
	}
	var sent []domain.Contributor
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("the create did not send a decodable contributor array: %s", raw)
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("create sent %#v, want %#v", sent, want)
	}

	// The keys are the wire format ADR 0016 names, and a renamed one silently
	// orphans every list already written into an inventory.
	var onWire []map[string]any
	if err := json.Unmarshal(raw, &onWire); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(onWire[0]))
	for key := range onWire[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := []string{"account_id", "association_id", "first_seen_batch", "identity",
		"last_seen_batch", "observed_at", "parent_id", "region", "resource_id",
		"source_file", "source_row", "type"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("contributor entry keys are %v, ADR 0016 names %v", keys, wantKeys)
	}
}

// A null observation time and a null association id must reach NetBox as JSON
// null, not as an absent key and not as "": ADR 0016 makes a null observed_at
// the thing that disqualifies an entry as removal evidence, and a key that
// simply vanished could not carry that.
func TestContributorNullsAreWrittenAsNull(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{}, created: map[string]any{
		"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
	}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()

	bare := testContributor("subnet-0abc")
	bare.Type = "subnet"
	bare.ParentID = "vpc-0aaa"
	bare.AssociationID = nil
	bare.ObservedAt = nil
	if _, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Batch: "batch-1", Contributors: []domain.Contributor{bare},
	}); err != nil {
		t.Fatal(err)
	}

	fields, _ := stack.bodies[0]["custom_fields"].(map[string]any)
	raw, _ := json.Marshal(fields[ImportContributorsField])
	var onWire []map[string]any
	if err := json.Unmarshal(raw, &onWire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"observed_at", "association_id"} {
		value, present := onWire[0][key]
		if !present {
			t.Fatalf("%s is absent from the written entry; ADR 0016 requires an explicit null: %s", key, raw)
		}
		if value != nil {
			t.Fatalf("%s is %#v, want null", key, value)
		}
	}
	if onWire[0]["parent_id"] != "vpc-0aaa" {
		t.Fatalf("parent_id is %#v, want the subnet's VPC", onWire[0]["parent_id"])
	}
}

// The create path is the only write path. ADR 0016 makes the refresh package
// M9b3's work, and until it exists an existing prefix must be left exactly as
// it is even when the table names contributors the prefix does not carry.
func TestEnsureOccupancyDoesNotWriteContributorsOntoAnExistingPrefix(t *testing.T) {
	stored := map[string]any{ImportBatchField: "batch-1", "operator_note": "keep me"}
	stack := &occupancyStack{prefixes: []any{map[string]any{
		"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
		"custom_fields": stored,
	}}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()

	got, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Batch: "batch-2", Source: "second-run.csv",
		Contributors: []domain.Contributor{testContributor("vpc-0new")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != OccupancyUnchanged {
		t.Fatalf("unexpected result %#v", got)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a re-import with new contributors wrote %v", writes)
	}
	if _, present := stored[ImportContributorsField]; present {
		t.Fatal("the existing prefix gained a contributor list; there is no update path in M9b1")
	}
}

func TestEnsureOccupancyRefusesContributorsOnARange(t *testing.T) {
	stack := &occupancyStack{ranges: []any{}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	_, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		StartAddress: "10.1.0.10", EndAddress: "10.1.0.20", Batch: "batch-1",
		Contributors: []domain.Contributor{testContributor("vpc-0aaa")},
	})
	if !errors.Is(err, ErrOccupancyInvalid) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusal still wrote %v", writes)
	}
}

func TestEnsureOccupancyRefusesAContributorWithoutAnIdentity(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	bad := testContributor("vpc-0aaa")
	bad.Identity = ""
	_, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Batch: "batch-1", Contributors: []domain.Contributor{bad},
	})
	if !errors.Is(err, ErrOccupancyInvalid) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if len(stack.requests) != 0 {
		t.Fatalf("refusal contacted NetBox: %v", stack.requests)
	}
}

// An empty list is not written. ADR 0016: "an empty list is not an argument
// that nothing contributes; it is a list nobody wrote", and a removal refuses
// on one -- so an import with nothing to record must leave the field absent,
// which is the pre-ADR-0016 shape, and not write [].
func TestEnsureOccupancyOmitsAnEmptyContributorList(t *testing.T) {
	for name, contributors := range map[string][]domain.Contributor{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			stack := &occupancyStack{prefixes: []any{}, created: map[string]any{
				"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
			}}
			c, d, closer := occupancyClient(t, stack)
			defer closer()
			if _, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
				CIDR: "10.1.0.0/16", Batch: "batch-1", Contributors: contributors,
			}); err != nil {
				t.Fatal(err)
			}
			fields, _ := stack.bodies[0]["custom_fields"].(map[string]any)
			if _, present := fields[ImportContributorsField]; present {
				t.Fatalf("an import with no contributors wrote %#v", fields[ImportContributorsField])
			}
		})
	}
}

// An installation whose NetBox has never been seeded with the field is the
// same case as one without the import tag, and behaves the same way: NetBox
// answers 400 ("Custom field ... does not exist for this object type",
// measured 2026-09-22), the adapter does not surface the body, and the import
// stops at that entry having written nothing. It must not succeed silently
// with the contributors dropped.
func TestEnsureOccupancyFailsOnANetBoxWithoutTheContributorField(t *testing.T) {
	var attempted int
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fields, _ := body["custom_fields"].(map[string]any)
			if _, present := fields[ImportContributorsField]; present {
				attempted++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"custom_fields":{"platform_import_contributors":"Custom field 'platform_import_contributors' does not exist for this object type."}}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":11,"prefix":"10.1.0.0/16","vrf":{"id":7}}`))
			return
		}
		w.Write(page(nil, ""))
	}))
	defer s.Close()
	d, p := testDomain()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Batch: "batch-1",
		Contributors: []domain.Contributor{testContributor("vpc-0aaa")},
	})
	if err == nil {
		t.Fatalf("an import against a NetBox without the field reported success: %#v", result)
	}
	if attempted != 1 {
		t.Fatalf("the create was attempted %d times, want once", attempted)
	}
	// The operator has to be able to tell which network failed. The adapter
	// deliberately never surfaces a NetBox body (HTTPError's own comment), so
	// the CIDR and the status are what it can say, and it must say both.
	if !strings.Contains(err.Error(), "10.1.0.0/16") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("error does not name the network and the status: %v", err)
	}
}

// --- the field is unowned -----------------------------------------------

// ADR 0016 rests on the contributor list surviving an adoption and an
// abandonment, and says in the same breath why that is tested rather than
// assumed: the AWS account and region were believed to survive an adoption
// until ADR 0012's review measured that they did not, because ownedFields
// owns them.
func TestOwnedFieldsDoNotOwnTheContributorList(t *testing.T) {
	a := adoptAllocation()
	a.Binding = &domain.Binding{ResourceID: "vpc-0abc"}
	owned := ownedFields(a, "op-1")
	// The import's other two unowned fields are the precedent this one joins.
	for _, unowned := range []string{ImportBatchField, ImportSourceField, ImportContributorsField} {
		if _, isOwned := owned[unowned]; isOwned {
			t.Fatalf("ownedFields owns %s; an adoption would overwrite the import's record and an abandonment would empty it", unowned)
		}
	}
}

func withContributors(x map[string]any, entries ...domain.Contributor) map[string]any {
	fields, _ := x["custom_fields"].(map[string]any)
	raw, err := json.Marshal(entries)
	if err != nil {
		panic(err)
	}
	var stored any
	if err := json.Unmarshal(raw, &stored); err != nil {
		panic(err)
	}
	fields[ImportContributorsField] = stored
	return x
}

func TestTheContributorListSurvivesAdoptionAbandonmentAndSync(t *testing.T) {
	want := []domain.Contributor{testContributor("vpc-0aaa"), testContributor("vpc-0bbb")}

	t.Run("adopt", func(t *testing.T) {
		stack := &adoptStack{etag: `W/"2026-09-22T10:00:00+00:00"`, prefixes: map[int]map[string]any{
			42: withContributors(importedPrefix(42, "10.0.1.0/24", 7), want...)}}
		c, closer := adoptClient(t, stack)
		defer closer()
		if _, err := c.Adopt(context.Background(), adoptAllocation(), "op-1"); err != nil {
			t.Fatal(err)
		}
		if got := contributorsOn(t, stack.prefixes[42]); !reflect.DeepEqual(got, want) {
			t.Fatalf("adoption changed the contributor list:\n got %#v\nwant %#v", got, want)
		}
		// Adopt preserves an unowned field by COPYING it out of the read and
		// back into the PATCH (adopt.go's merge loop), not by omitting it, so
		// the key does appear in the body. What must hold is that the value
		// makes the round trip untouched: a lossy copy would rewrite the
		// evidence rather than lose it, which is worse.
		var sawIt bool
		for _, body := range stack.bodies {
			fields, _ := body["custom_fields"].(map[string]any)
			value, present := fields[ImportContributorsField]
			if !present {
				continue
			}
			sawIt = true
			got, unreadable := contributorsCF(map[string]any{ImportContributorsField: value}, ImportContributorsField)
			if unreadable || !reflect.DeepEqual(got, want) {
				t.Fatalf("an adoption's PATCH altered the contributor list:\n got %#v (unreadable=%v)\nwant %#v", got, unreadable, want)
			}
		}
		if !sawIt {
			t.Fatal("the adoption's PATCH carried no contributor list; adopt.go preserves unowned fields by re-sending them, so its absence would clear the field")
		}
	})

	t.Run("abandon", func(t *testing.T) {
		a := adoptAllocation()
		owned := ownedBy(42, "10.0.1.0/24", map[string]any{
			allocationIDCF: a.ID, allocationKeyCF: a.AllocationKey,
			operationIDCF: "op-1", stateCF: domain.Reserved,
		})
		stack := &adoptStack{etag: `W/"2026-09-22T10:00:00+00:00"`,
			prefixes: map[int]map[string]any{42: withContributors(owned, want...)}}
		c, closer := adoptClient(t, stack)
		defer closer()
		if err := c.AbandonAdoption(context.Background(), a, "op-1",
			domain.PriorInventory{Status: "active", AccountID: "123456789012", Region: "eu-central-1"}); err != nil {
			t.Fatal(err)
		}
		if got := contributorsOn(t, stack.prefixes[42]); !reflect.DeepEqual(got, want) {
			t.Fatalf("abandonment changed the contributor list:\n got %#v\nwant %#v", got, want)
		}
	})

	t.Run("sync", func(t *testing.T) {
		a := adoptAllocation()
		a.InventoryID = "42"
		owned := ownedBy(42, "10.0.1.0/24", map[string]any{
			allocationIDCF: a.ID, allocationKeyCF: a.AllocationKey,
			operationIDCF: "op-1", stateCF: domain.Reserved,
		})
		stack := &adoptStack{etag: `W/"2026-09-22T10:00:00+00:00"`,
			prefixes: map[int]map[string]any{42: withContributors(owned, want...)}}
		c, closer := adoptClient(t, stack)
		defer closer()
		if err := c.Sync(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if got := contributorsOn(t, stack.prefixes[42]); !reflect.DeepEqual(got, want) {
			t.Fatalf("a sync changed the contributor list:\n got %#v\nwant %#v", got, want)
		}
	})
}

// --- reading the list back ----------------------------------------------

func TestContributorsCFDistinguishesAbsentEmptyAndUnreadable(t *testing.T) {
	entries := []domain.Contributor{testContributor("vpc-0aaa")}
	raw, _ := json.Marshal(entries)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)

	for name, tc := range map[string]struct {
		fields     map[string]any
		want       []domain.Contributor
		wantNil    bool
		unreadable bool
	}{
		// An unset NetBox custom field is the key present with JSON null, not
		// a missing key -- the shape package C5 found the hard way.
		"unset":            {fields: map[string]any{ImportContributorsField: nil}, wantNil: true},
		"absent key":       {fields: map[string]any{}, wantNil: true},
		"no custom fields": {fields: nil, wantNil: true},
		"populated":        {fields: map[string]any{ImportContributorsField: decoded}, want: entries},
		// An empty array is a list somebody wrote that says nothing, and ADR
		// 0016 refuses a removal on it. It must not read as absence.
		"empty array": {fields: map[string]any{ImportContributorsField: []any{}}, want: []domain.Contributor{}},
		// Whatever this is, it is not a contributor list, and guessing is the
		// one thing the record forbids.
		"an operator's object": {fields: map[string]any{ImportContributorsField: map[string]any{"note": "mine"}}, unreadable: true},
		"a string":             {fields: map[string]any{ImportContributorsField: "vpc-0aaa"}, unreadable: true},
		"a number":             {fields: map[string]any{ImportContributorsField: float64(3)}, unreadable: true},
		"entries of the wrong shape": {fields: map[string]any{
			ImportContributorsField: []any{map[string]any{"identity": float64(7)}}}, unreadable: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, unreadable := contributorsCF(tc.fields, ImportContributorsField)
			if unreadable != tc.unreadable {
				t.Fatalf("unreadable = %v, want %v (got %#v)", unreadable, tc.unreadable, got)
			}
			if tc.unreadable {
				if got != nil {
					t.Fatalf("an unreadable field returned entries: %#v", got)
				}
				return
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected a nil slice for %s, got %#v", name, got)
				}
				return
			}
			if got == nil {
				t.Fatal("a readable list must not decode to nil: absence and an empty list lead to opposite decisions")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSnapshotCarriesContributorsOutOfTheInventory(t *testing.T) {
	entries := []domain.Contributor{testContributor("vpc-0aaa"), testContributor("vpc-0bbb")}
	raw, _ := json.Marshal(entries)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)

	d, p := testDomain()
	prefixes := []any{
		map[string]any{"id": 1, "prefix": p.CIDR, "vrf": map[string]any{"id": 7},
			"status": "container", "custom_fields": map[string]any{}},
		map[string]any{"id": 11, "prefix": "10.0.1.0/24", "vrf": map[string]any{"id": 7},
			"tags":          []any{map[string]any{"slug": ImportedTag}},
			"custom_fields": map[string]any{ImportBatchField: "batch-1", ImportContributorsField: decoded}},
		map[string]any{"id": 12, "prefix": "10.0.2.0/24", "vrf": map[string]any{"id": 7},
			"tags":          []any{map[string]any{"slug": ImportedTag}},
			"custom_fields": map[string]any{ImportBatchField: "batch-0", ImportContributorsField: nil}},
		map[string]any{"id": 13, "prefix": "10.0.3.0/24", "vrf": map[string]any{"id": 7},
			"tags":          []any{map[string]any{"slug": ImportedTag}},
			"custom_fields": map[string]any{ImportContributorsField: "an operator wrote this"}},
	}
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ipam/vrfs/":
			w.Write(page([]any{map[string]any{"id": 7, "name": "platform"}}, ""))
		case "/api/ipam/prefixes/":
			w.Write(page(prefixes, ""))
		default:
			w.Write(page(nil, ""))
		}
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	byCIDR := map[string]domain.Network{}
	for _, n := range snap.Networks {
		byCIDR[n.CIDR] = n
	}

	if got := byCIDR["10.0.1.0/24"]; !reflect.DeepEqual(got.Contributors, entries) || got.ContributorsUnreadable {
		t.Fatalf("a populated prefix came back as %#v (unreadable=%v)", got.Contributors, got.ContributorsUnreadable)
	}
	if got := byCIDR["10.0.2.0/24"]; got.Contributors != nil || got.ContributorsUnreadable {
		t.Fatalf("a prefix imported before ADR 0016 must carry no list: %#v (unreadable=%v)", got.Contributors, got.ContributorsUnreadable)
	}
	if got := byCIDR["10.0.3.0/24"]; got.Contributors != nil || !got.ContributorsUnreadable {
		t.Fatalf("an unreadable list must be reported, not silently empty: %#v (unreadable=%v)", got.Contributors, got.ContributorsUnreadable)
	}
	// One malformed field must never take the domain offline: a failed
	// Snapshot answers 503 for every reservation in it.
	if !snap.Complete {
		t.Fatal("an unreadable contributor list made the snapshot incomplete")
	}
}
