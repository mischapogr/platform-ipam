package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// adoptStack is a NetBox holding one prefix table. It records every request so
// a test can assert what was written and, more often, that nothing was.
type adoptStack struct {
	prefixes    map[int]map[string]any
	etag        string                 // served on every detail read; empty means the release offers none
	patchStatus int                    // when set, the status a PATCH answers with instead of applying
	afterPatch  func(x map[string]any) // a foreign write landing between the PATCH and the re-read
	beforeRead  func(x map[string]any) // a foreign write landing between the list read and the detail read; runs once
	patchDelay  time.Duration          // a write that hangs: the adapter's answer says nothing about it
	timeout     time.Duration          // the client's own timeout; zero leaves the adapter's default
	// deleteStatus, deleteDelay and afterDelete are the DELETE's counterparts
	// of the three above, added for CancelReservation (ADR 0013), which is the
	// one port operation that destroys an object. afterDelete runs once the
	// object is gone, so a test can put something back at that id and exercise
	// the confirmation read the cancel makes before it answers.
	deleteStatus int
	deleteDelay  time.Duration
	afterDelete  func(s *adoptStack)
	// readStatus, when set, is what every detail read answers with; a test sets
	// it from afterDelete to fail the confirmation read and nothing before it.
	readStatus int
	requests   []string
	bodies     []map[string]any
	ifMatch    []string
}

func (s *adoptStack) server(t *testing.T) *httptest.Server {
	t.Helper()
	return localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		id, detail := detailPrefixID(r.URL.Path)
		switch {
		case detail && r.Method == http.MethodGet:
			if s.readStatus != 0 {
				w.WriteHeader(s.readStatus)
				return
			}
			x, ok := s.prefixes[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if s.beforeRead != nil {
				s.beforeRead(x)
				s.beforeRead = nil
			}
			if s.etag != "" {
				w.Header().Set("ETag", s.etag)
			}
			_ = json.NewEncoder(w).Encode(x)
		case detail && r.Method == http.MethodPatch:
			if s.patchDelay > 0 {
				time.Sleep(s.patchDelay)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("write body is not JSON: %v", err)
			}
			s.bodies = append(s.bodies, body)
			s.ifMatch = append(s.ifMatch, r.Header.Get("If-Match"))
			if s.patchStatus != 0 {
				w.WriteHeader(s.patchStatus)
				return
			}
			s.apply(id, body)
			_ = json.NewEncoder(w).Encode(s.prefixes[id])
		// NetBox 4.6.7 honours If-Match on a prefix DELETE and answers 204 on
		// success, 412 when the ETag has moved on and 404 for an id that is
		// already gone (measured for package H8a; see ADR 0013). The header is
		// recorded rather than compared for the reason the PATCH branch records
		// it: a test that wants the conditional refusal asks for it by status.
		case detail && r.Method == http.MethodDelete:
			if s.deleteDelay > 0 {
				time.Sleep(s.deleteDelay)
			}
			s.ifMatch = append(s.ifMatch, r.Header.Get("If-Match"))
			if s.deleteStatus != 0 {
				w.WriteHeader(s.deleteStatus)
				return
			}
			if _, ok := s.prefixes[id]; !ok {
				http.NotFound(w, r)
				return
			}
			delete(s.prefixes, id)
			if s.afterDelete != nil {
				s.afterDelete(s)
			}
			w.WriteHeader(http.StatusNoContent)
		// Ensure's create, so that a cancel can be shown against a prefix this
		// package really wrote rather than one a fixture describes.
		case r.Method == http.MethodPost && r.URL.Path == "/api/ipam/prefixes/":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("create body is not JSON: %v", err)
			}
			s.bodies = append(s.bodies, body)
			created := s.create(body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		case r.Method == http.MethodGet && r.URL.Path == "/api/ipam/prefixes/":
			w.Write(page(s.matching(r.URL.Query()), ""))
		default:
			http.NotFound(w, r)
		}
	}))
}

// create stores what a POST asked for and gives it the next id, the way NetBox
// does. A created prefix carries no tags, which is exactly what separates the
// object Ensure writes from the one an import leaves.
func (s *adoptStack) create(body map[string]any) map[string]any {
	next := 1
	for id := range s.prefixes {
		if id >= next {
			next = id + 1
		}
	}
	x := map[string]any{"id": next, "tags": []any{}}
	for key, value := range body {
		x[key] = value
	}
	if s.prefixes == nil {
		s.prefixes = map[int]map[string]any{}
	}
	s.prefixes[next] = x
	return x
}

func detailPrefixID(path string) (int, bool) {
	const collection = "/api/ipam/prefixes/"
	if !strings.HasPrefix(path, collection) {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, collection), "/"))
	return id, err == nil
}

// matching answers the two list reads an adoption makes -- by CIDR and by
// allocation marker -- so the adapter's own second, local filter is exercised
// rather than assumed.
func (s *adoptStack) matching(q url.Values) []any {
	ids := make([]int, 0, len(s.prefixes))
	for id := range s.prefixes {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var out []any
	for _, id := range ids {
		x := s.prefixes[id]
		if want := q.Get("prefix"); want != "" && x["prefix"] != want {
			continue
		}
		if want := q.Get("cf_" + allocationIDCF); want != "" {
			fields, _ := x["custom_fields"].(map[string]any)
			if fields[allocationIDCF] != want {
				continue
			}
		}
		out = append(out, x)
	}
	return out
}

// apply mutates the stored object the way NetBox 4.6.7 does: custom_fields are
// merged key by key, every other key replaces, and a tag list absent from the
// body is left alone. Established against the development NetBox; a PATCH that
// did carry a tags list would replace the import tag instead.
func (s *adoptStack) apply(id int, body map[string]any) {
	x := s.prefixes[id]
	for key, value := range body {
		if key == "custom_fields" {
			patch, _ := value.(map[string]any)
			fields, _ := x["custom_fields"].(map[string]any)
			if fields == nil {
				fields = map[string]any{}
			}
			for name, v := range patch {
				fields[name] = v
			}
			x["custom_fields"] = fields
			continue
		}
		x[key] = value
	}
	if s.afterPatch != nil {
		s.afterPatch(x)
	}
}

func (s *adoptStack) writes() []string {
	var out []string
	for _, request := range s.requests {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			out = append(out, request)
		}
	}
	return out
}

func adoptClient(t *testing.T, stack *adoptStack) (*Client, func()) {
	t.Helper()
	d, p := testDomain()
	s := stack.server(t)
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}, Timeout: stack.timeout})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return c, s.Close
}

// importedPrefix is what an onboarding import leaves behind: the import tag,
// the batch and the source, and every other custom field null -- the shape a
// real NetBox returns, where an unset field is JSON null and not a missing key.
func importedPrefix(id int, cidr string, vrfID int) map[string]any {
	return map[string]any{
		"id": id, "prefix": cidr, "vrf": map[string]any{"id": vrfID},
		"status": map[string]any{"value": "active", "label": "Active"},
		"tags":   []any{map[string]any{"slug": ImportedTag}},
		"custom_fields": map[string]any{
			ImportBatchField: "batch-1", ImportSourceField: "networks.csv",
			awsAccountCF: "123456789012", awsRegionCF: "eu-central-1", awsResourceCF: "vpc-0abc",
			"operator_note": "keep me",
			allocationIDCF:  nil, allocationKeyCF: nil, operationIDCF: nil, stateCF: nil,
		},
	}
}

func ownedBy(id int, cidr string, fields map[string]any) map[string]any {
	x := importedPrefix(id, cidr, 7)
	stored, _ := x["custom_fields"].(map[string]any)
	for key, value := range fields {
		stored[key] = value
	}
	return x
}

func adoptAllocation() domain.Allocation {
	return domain.Allocation{
		ID: "alloc-1", DomainID: "connected", PoolID: "pool", CIDR: "10.0.1.0/24",
		TenantID: "orders-team", State: domain.Reserved, PolicyVersion: "v1",
		Request: domain.Request{AllocationKey: "orders", Scope: "vpc", Environment: "prod",
			Region: "eu-central-1", AccountID: "123456789012", PrefixLength: 24},
	}
}

func TestAdoptConvertsTheImportedPrefix(t *testing.T) {
	stack := &adoptStack{etag: `W/"2026-09-18T19:53:45.017476+00:00"`,
		prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	a := adoptAllocation()
	id, err := c.Adopt(context.Background(), a, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Fatalf("inventory ID is %q, want the NetBox ID of the reviewed prefix", id)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "PATCH /api/ipam/prefixes/42/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	if stack.ifMatch[0] != stack.etag {
		t.Fatalf("the write was not conditional: If-Match %q, ETag %q", stack.ifMatch[0], stack.etag)
	}
	body := stack.bodies[0]
	if body["status"] != "reserved" {
		t.Fatalf("status is %#v, want the reserved Ensure creates", body["status"])
	}
	// A tags list in the body would replace the prefix's tags, taking the
	// import provenance with it.
	if _, present := body["tags"]; present {
		t.Fatalf("the write carries a tags list: %#v", body["tags"])
	}
	fields, _ := body["custom_fields"].(map[string]any)
	for key, want := range map[string]string{
		allocationIDCF: a.ID, allocationKeyCF: a.AllocationKey, operationIDCF: "op-1", stateCF: a.State,
	} {
		if fields[key] != want {
			t.Fatalf("owned field %s is %#v, want %q", key, fields[key], want)
		}
	}
	for key, want := range map[string]string{
		ImportBatchField: "batch-1", ImportSourceField: "networks.csv",
		awsResourceCF: "vpc-0abc", "operator_note": "keep me",
	} {
		if fields[key] != want {
			t.Fatalf("unowned field %s is %#v, want %q", key, fields[key], want)
		}
	}
	after := stack.prefixes[42]
	tags, _ := after["tags"].([]any)
	if tag, _ := tags[0].(map[string]any); len(tags) != 1 || tag["slug"] != ImportedTag {
		t.Fatalf("the import tag did not survive: %#v", after["tags"])
	}
	stored, _ := after["custom_fields"].(map[string]any)
	if stored[ImportBatchField] != "batch-1" || stored[ImportSourceField] != "networks.csv" {
		t.Fatalf("the import batch and source did not survive: %#v", stored)
	}
}

func TestAdoptRepeatsWithoutWriting(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	a := adoptAllocation()
	if _, err := c.Adopt(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	stack.requests, stack.bodies, stack.ifMatch = nil, nil, nil
	// A crash between the adapter write and the ledger commit re-runs this, and
	// converging on the prefix already written is what lets the operation close.
	id, err := c.Adopt(context.Background(), a, "op-1")
	if err != nil {
		t.Fatalf("a repeated adoption must converge: %v", err)
	}
	if id != "42" {
		t.Fatalf("inventory ID %q", id)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated adoption wrote %v", writes)
	}
}

func TestAdoptRefusesItsOwnPrefixUnderAnotherOperation(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	a := adoptAllocation()
	if _, err := c.Adopt(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	stack.requests, stack.bodies = nil, nil
	if _, err := c.Adopt(context.Background(), a, "op-2"); !errors.Is(err, ErrAdoptConflict) {
		t.Fatalf("got %v, want %v", err, ErrAdoptConflict)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
}

// The allocation marker and the operation marker both match here, so only the
// allocation key distinguishes this prefix from the one the ledger means. A
// replay that ignored it would hand back an inventory ID for another key.
func TestAdoptRefusesItsOwnPrefixUnderAnotherAllocationKey(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	a := adoptAllocation()
	if _, err := c.Adopt(context.Background(), a, "op-1"); err != nil {
		t.Fatal(err)
	}
	stack.requests, stack.bodies = nil, nil
	a.AllocationKey = "payments"
	if _, err := c.Adopt(context.Background(), a, "op-1"); !errors.Is(err, ErrAdoptConflict) {
		t.Fatalf("got %v, want %v", err, ErrAdoptConflict)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a refusal wrote %v", writes)
	}
}

func TestAdoptRefusesEverythingButOneReviewedImportedPrefix(t *testing.T) {
	untagged := importedPrefix(42, "10.0.1.0/24", 7)
	untagged["tags"] = []any{}
	cases := map[string]struct {
		prefixes map[int]map[string]any
		want     error
	}{
		"another allocation owns it": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{allocationIDCF: "alloc-2", allocationKeyCF: "billing"}),
		}, ErrAdoptManaged},
		"a stray operation marker": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{operationIDCF: "op-9"}),
		}, ErrAdoptManaged},
		"a stray state marker": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{stateCF: domain.Reserved}),
		}, ErrAdoptManaged},
		"a stray allocation key": {map[int]map[string]any{
			42: ownedBy(42, "10.0.1.0/24", map[string]any{allocationKeyCF: "orders"}),
		}, ErrAdoptManaged},
		"no import tag":       {map[int]map[string]any{42: untagged}, ErrAdoptNotImported},
		"nothing at the CIDR": {map[int]map[string]any{}, ErrAdoptNotFound},
		// A prefix outside the pool's VRF is invisible to Snapshot, so the
		// allocator would never see what the allocation claims to own.
		"the prefix is in another VRF": {map[int]map[string]any{
			42: importedPrefix(42, "10.0.1.0/24", 9),
		}, ErrAdoptNotFound},
		"two prefixes hold the CIDR": {map[int]map[string]any{
			42: importedPrefix(42, "10.0.1.0/24", 7), 43: importedPrefix(43, "10.0.1.0/24", 7),
		}, ErrAdoptConflict},
		"another prefix already claims the allocation": {map[int]map[string]any{
			42: importedPrefix(42, "10.0.1.0/24", 7),
			43: ownedBy(43, "10.0.2.0/24", map[string]any{allocationIDCF: "alloc-1"}),
		}, ErrAdoptConflict},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{prefixes: tc.prefixes}
			c, closer := adoptClient(t, stack)
			defer closer()
			if _, err := c.Adopt(context.Background(), adoptAllocation(), "op-1"); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if writes := stack.writes(); len(writes) != 0 {
				t.Fatalf("a refusal wrote %v", writes)
			}
		})
	}
}

func TestAdoptRefusesACIDRBeforeContactingNetBox(t *testing.T) {
	for name, cidr := range map[string]string{
		"the pool itself":      "10.0.0.0/16",
		"a container of it":    "10.0.0.0/8",
		"outside the pool":     "10.9.0.0/24",
		"a non-canonical CIDR": "10.0.1.5/24",
		"IPv6":                 "2001:db8::/32",
	} {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{prefixes: map[int]map[string]any{}}
			c, closer := adoptClient(t, stack)
			defer closer()
			a := adoptAllocation()
			a.CIDR = cidr
			if _, err := c.Adopt(context.Background(), a, "op-1"); !errors.Is(err, ErrAdoptInvalid) {
				t.Fatalf("got %v, want %v", err, ErrAdoptInvalid)
			}
			if len(stack.requests) != 0 {
				t.Fatalf("a refusal contacted NetBox: %v", stack.requests)
			}
		})
	}
	stack := &adoptStack{prefixes: map[int]map[string]any{}}
	c, closer := adoptClient(t, stack)
	defer closer()
	if _, err := c.Adopt(context.Background(), adoptAllocation(), ""); !errors.Is(err, ErrAdoptInvalid) {
		t.Fatalf("an adoption without an operation ID must refuse, got %v", err)
	}
	if len(stack.requests) != 0 {
		t.Fatalf("a refusal contacted NetBox: %v", stack.requests)
	}
}

func TestAdoptRefusesAReadBackThatIsNotThisAllocations(t *testing.T) {
	for name, corrupt := range map[string]func(x map[string]any){
		"a different owner": func(x map[string]any) {
			fields, _ := x["custom_fields"].(map[string]any)
			fields[allocationIDCF] = "alloc-2"
		},
		"the import tag gone": func(x map[string]any) { x["tags"] = []any{} },
	} {
		t.Run(name, func(t *testing.T) {
			stack := &adoptStack{etag: `W/"1"`, afterPatch: corrupt,
				prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
			c, closer := adoptClient(t, stack)
			defer closer()
			if _, err := c.Adopt(context.Background(), adoptAllocation(), "op-1"); !errors.Is(err, ErrAdoptReadBack) {
				t.Fatalf("got %v, want %v", err, ErrAdoptReadBack)
			}
			// The write has already happened. A second one would overwrite
			// whoever holds the prefix now; the operation stays pending instead.
			if writes := stack.writes(); len(writes) != 1 {
				t.Fatalf("expected one PATCH and no retry, got %v", writes)
			}
		})
	}
}

func TestAdoptRefusesWhenThePrefixChangedUnderTheConditionalWrite(t *testing.T) {
	stack := &adoptStack{etag: `W/"1"`, patchStatus: http.StatusPreconditionFailed,
		prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	if _, err := c.Adopt(context.Background(), adoptAllocation(), "op-1"); !errors.Is(err, ErrAdoptConflict) {
		t.Fatalf("got %v, want %v", err, ErrAdoptConflict)
	}
	if writes := stack.writes(); len(writes) != 1 {
		t.Fatalf("expected one refused PATCH and no retry, got %v", writes)
	}
	fields, _ := stack.prefixes[42]["custom_fields"].(map[string]any)
	if fields[allocationIDCF] != nil {
		t.Fatalf("a refused conditional write still changed the prefix: %#v", fields)
	}
}

// A NetBox that offers no ETag falls back to the unconditional write ADR 0010
// designed; the re-read, not the header, is what catches a foreign change
// there, so the fallback must still adopt.
func TestAdoptWritesUnconditionallyWithoutAnETag(t *testing.T) {
	stack := &adoptStack{prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
	c, closer := adoptClient(t, stack)
	defer closer()
	id, err := c.Adopt(context.Background(), adoptAllocation(), "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Fatalf("inventory ID %q", id)
	}
	if stack.ifMatch[0] != "" {
		t.Fatalf("If-Match was sent without an ETag to match: %q", stack.ifMatch[0])
	}
}

// Real NetBox renders every unset custom field as null rather than omitting the
// key. Reading null as the string "<nil>" would make every imported prefix look
// owned, and refuse every adoption.
func TestAdoptTreatsNullOwnershipFieldsAsUnset(t *testing.T) {
	x := importedPrefix(42, "10.0.1.0/24", 7)
	fields, _ := x["custom_fields"].(map[string]any)
	for _, key := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		value, present := fields[key]
		if !present || value != nil {
			t.Fatalf("the fixture is not the null shape NetBox returns: %#v", fields)
		}
	}
	stack := &adoptStack{prefixes: map[int]map[string]any{42: x}}
	c, closer := adoptClient(t, stack)
	defer closer()
	if _, err := c.Adopt(context.Background(), adoptAllocation(), "op-1"); err != nil {
		t.Fatalf("a prefix whose ownership fields are null must adopt: %v", err)
	}
}
