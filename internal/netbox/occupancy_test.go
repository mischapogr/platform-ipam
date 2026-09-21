package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// occupancyStack answers the reads an import makes and records every write, so
// a test can assert both what was sent and that nothing was sent at all.
type occupancyStack struct {
	prefixes, ranges []any
	created          map[string]any
	requests         []string
	bodies           []map[string]any
}

func (o *occupancyStack) server(t *testing.T) *httptest.Server {
	t.Helper()
	return localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		o.requests = append(o.requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("write body is not JSON: %v", err)
			}
			o.bodies = append(o.bodies, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(o.created)
			return
		}
		switch r.URL.Path {
		case "/api/ipam/prefixes/":
			w.Write(page(o.prefixes, ""))
		case "/api/ipam/ip-ranges/":
			w.Write(page(o.ranges, ""))
		default:
			http.NotFound(w, r)
		}
	}))
}

func (o *occupancyStack) writes() []string {
	var out []string
	for _, request := range o.requests {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			out = append(out, request)
		}
	}
	return out
}

func occupancyClient(t *testing.T, stack *occupancyStack) (*Client, domain.Domain, func()) {
	t.Helper()
	d, p := testDomain()
	s := stack.server(t)
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return c, d, s.Close
}

func TestEnsureOccupancyCreatesUnmanagedPrefixInTheDomainVRF(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{}, created: map[string]any{
		"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
		"custom_fields": map[string]any{ImportBatchField: "batch-1"},
	}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	got, err := c.EnsureOccupancy(context.Background(), d, Occupancy{
		CIDR: "10.1.0.0/16", Description: "legacy VPC", Batch: "batch-1", Source: "networks.csv",
		AWSAccountID: "123456789012", AWSRegion: "eu-central-1", AWSResourceID: "vpc-0abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != (OccupancyResult{Kind: OccupancyPrefix, ID: "11", Action: OccupancyCreated}) {
		t.Fatalf("unexpected result %#v", got)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "POST /api/ipam/prefixes/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	body := stack.bodies[0]
	if body["vrf"] != float64(7) || body["status"] != "active" || body["prefix"] != "10.1.0.0/16" {
		t.Fatalf("prefix was not created in the domain VRF: %#v", body)
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("expected exactly the import tag: %#v", body["tags"])
	}
	if tag, _ := tags[0].(map[string]any); tag["slug"] != ImportedTag {
		t.Fatalf("import tag missing: %#v", tags[0])
	}
	fields, _ := body["custom_fields"].(map[string]any)
	for key, want := range map[string]string{
		ImportBatchField: "batch-1", ImportSourceField: "networks.csv",
		awsAccountCF: "123456789012", awsRegionCF: "eu-central-1", awsResourceCF: "vpc-0abc",
	} {
		if fields[key] != want {
			t.Fatalf("custom field %s is %#v, want %q", key, fields[key], want)
		}
	}
	// The end-to-end suite asserts that every prefix carrying an allocation id
	// is known to the API; an import must never put one there.
	for _, forbidden := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		if _, present := fields[forbidden]; present {
			t.Fatalf("import wrote the managed field %s: %#v", forbidden, fields)
		}
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "platform_allocation") || strings.Contains(string(raw), stateCF) {
		t.Fatalf("request body mentions a managed marker: %s", raw)
	}
}

func TestEnsureOccupancyRepeatsWithoutWriting(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{map[string]any{
		"id": 11, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
		"custom_fields": map[string]any{ImportBatchField: "batch-1", "operator_note": "keep me"},
	}}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	got, err := c.EnsureOccupancy(context.Background(), d, Occupancy{CIDR: "10.1.0.0/16", Batch: "batch-2", Source: "second-run.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if got != (OccupancyResult{Kind: OccupancyPrefix, ID: "11", Action: OccupancyUnchanged}) {
		t.Fatalf("unexpected result %#v", got)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated import wrote %v", writes)
	}
}

func TestEnsureOccupancyRefusesAManagedPrefix(t *testing.T) {
	stack := &occupancyStack{prefixes: []any{map[string]any{
		"id": 42, "prefix": "10.1.0.0/16", "vrf": map[string]any{"id": 7},
		"custom_fields": map[string]any{allocationIDCF: "alloc-1", operationIDCF: "op-1"},
	}}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	_, err := c.EnsureOccupancy(context.Background(), d, Occupancy{CIDR: "10.1.0.0/16", Batch: "batch-1"})
	if !errors.Is(err, ErrOccupancyManaged) {
		t.Fatalf("expected a managed-object refusal, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusal still wrote %v", writes)
	}
}

func TestEnsureOccupancyRefusesPoolSpace(t *testing.T) {
	for name, tc := range map[string]struct {
		cidr string
		want error
	}{
		"the pool itself":       {"10.0.0.0/16", ErrOccupancyPool},
		"a container of it":     {"10.0.0.0/8", ErrOccupancyPool},
		"a non-canonical CIDR":  {"10.1.2.3/16", ErrOccupancyInvalid},
		"IPv6":                  {"2001:db8::/32", ErrOccupancyInvalid},
		"an IPv4-mapped prefix": {"::ffff:10.1.0.0/112", ErrOccupancyInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			stack := &occupancyStack{}
			c, d, closer := occupancyClient(t, stack)
			defer closer()
			_, err := c.EnsureOccupancy(context.Background(), d, Occupancy{CIDR: tc.cidr, Batch: "batch-1"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v for %s, want %v", err, tc.cidr, tc.want)
			}
			if len(stack.requests) != 0 {
				t.Fatalf("refusal contacted NetBox: %v", stack.requests)
			}
		})
	}
}

func TestEnsureOccupancyRequiresAConfiguredVRF(t *testing.T) {
	stack := &occupancyStack{}
	s := stack.server(t)
	defer s.Close()
	d := domain.Domain{ID: "connected", CoverageGeneration: "g1"}
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureOccupancy(context.Background(), d, Occupancy{CIDR: "10.1.0.0/16", Batch: "batch-1"}); !errors.Is(err, ErrOccupancyNoVRF) {
		t.Fatalf("expected a VRF refusal, got %v", err)
	}
	// A domain the client does not serve must not be written either: the VRF
	// would come from the caller rather than from configuration.
	other := domain.Domain{ID: "other", Backend: domain.Backend{VRFID: 9}}
	if _, err := c.EnsureOccupancy(context.Background(), other, Occupancy{CIDR: "10.1.0.0/16", Batch: "batch-1"}); err == nil || !strings.Contains(err.Error(), "unknown domain") {
		t.Fatalf("expected an unknown-domain refusal, got %v", err)
	}
	if len(stack.requests) != 0 {
		t.Fatalf("refusal contacted NetBox: %v", stack.requests)
	}
}

func TestEnsureOccupancyCreatesAndRepeatsARange(t *testing.T) {
	stack := &occupancyStack{ranges: []any{}, created: map[string]any{
		"id": 5, "start_address": "10.2.0.10/32", "end_address": "10.2.0.20/32", "vrf": 7,
	}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	o := Occupancy{StartAddress: "10.2.0.10", EndAddress: "10.2.0.20", Description: "network team range", Batch: "batch-1", Source: "ranges.csv"}
	got, err := c.EnsureOccupancy(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	if got != (OccupancyResult{Kind: OccupancyRange, ID: "5", Action: OccupancyCreated}) {
		t.Fatalf("unexpected result %#v", got)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "POST /api/ipam/ip-ranges/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	body := stack.bodies[0]
	if body["vrf"] != float64(7) || body["start_address"] != "10.2.0.10" || body["end_address"] != "10.2.0.20" {
		t.Fatalf("range was not created as requested: %#v", body)
	}
	tags, _ := body["tags"].([]any)
	if tag, _ := tags[0].(map[string]any); len(tags) != 1 || tag["slug"] != ImportedTag {
		t.Fatalf("import tag missing: %#v", body["tags"])
	}
	fields, _ := body["custom_fields"].(map[string]any)
	if fields[ImportBatchField] != "batch-1" || fields[ImportSourceField] != "ranges.csv" || len(fields) != 2 {
		t.Fatalf("a range carries only the import fields: %#v", fields)
	}

	// NetBox echoes addresses with a mask; a repeat must still recognise them.
	stack.ranges = []any{map[string]any{"id": 5, "start_address": "10.2.0.10/32", "end_address": "10.2.0.20/32", "vrf": map[string]any{"id": 7}}}
	stack.requests, stack.bodies = nil, nil
	got, err = c.EnsureOccupancy(context.Background(), d, o)
	if err != nil {
		t.Fatal(err)
	}
	if got != (OccupancyResult{Kind: OccupancyRange, ID: "5", Action: OccupancyUnchanged}) {
		t.Fatalf("unexpected result %#v", got)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated range import wrote %v", writes)
	}
}

func TestEnsureOccupancyRefusesARangeThatWouldBlockAPool(t *testing.T) {
	stack := &occupancyStack{ranges: []any{}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	// Snapshot covers a range with prefixes and applies no ancestor rule, so
	// this range would mark the whole pool occupied.
	_, err := c.EnsureOccupancy(context.Background(), d, Occupancy{StartAddress: "10.0.0.0", EndAddress: "10.0.255.255", Batch: "batch-1"})
	if !errors.Is(err, ErrOccupancyPool) {
		t.Fatalf("expected a pool refusal, got %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusal still wrote %v", writes)
	}
}

func TestEnsureOccupancyRefusesOverlappingAndMalformedRows(t *testing.T) {
	stack := &occupancyStack{ranges: []any{map[string]any{"id": 5, "start_address": "10.2.0.10/32", "end_address": "10.2.0.20/32", "vrf": 7}}}
	c, d, closer := occupancyClient(t, stack)
	defer closer()
	cases := map[string]struct {
		o    Occupancy
		want error
	}{
		"overlapping range":   {Occupancy{StartAddress: "10.2.0.15", EndAddress: "10.2.0.30", Batch: "b"}, ErrOccupancyConflict},
		"AWS fields on range": {Occupancy{StartAddress: "10.3.0.1", EndAddress: "10.3.0.2", Batch: "b", AWSAccountID: "123456789012"}, ErrOccupancyInvalid},
		"reversed range":      {Occupancy{StartAddress: "10.3.0.9", EndAddress: "10.3.0.1", Batch: "b"}, ErrOccupancyInvalid},
		"both forms":          {Occupancy{CIDR: "10.1.0.0/16", StartAddress: "10.3.0.1", EndAddress: "10.3.0.2", Batch: "b"}, ErrOccupancyInvalid},
		"neither form":        {Occupancy{Batch: "b"}, ErrOccupancyInvalid},
		"no batch":            {Occupancy{CIDR: "10.1.0.0/16"}, ErrOccupancyInvalid},
		"long description":    {Occupancy{CIDR: "10.1.0.0/16", Batch: "b", Description: strings.Repeat("x", maxDescription+1)}, ErrOccupancyInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.EnsureOccupancy(context.Background(), d, tc.o); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusals still wrote %v", writes)
	}
}
