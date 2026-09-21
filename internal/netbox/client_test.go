package netbox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func testDomain() (domain.Domain, domain.Pool) {
	return domain.Domain{ID: "connected", CoverageGeneration: "g1", Backend: domain.Backend{VRFID: 7, RequireUniquePrefixes: true}}, domain.Pool{ID: "pool", DomainID: "connected", CIDR: "10.0.0.0/16", Backend: domain.Backend{VRFID: 7}}
}

func page(results any, next any) []byte {
	if next == "" {
		next = nil
	}
	b, _ := json.Marshal(map[string]any{"count": 1, "next": next, "results": results})
	return b
}

func localServer(h http.Handler) *httptest.Server {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &httptest.Server{Listener: listener, Config: &http.Server{Handler: h}}
	s.Start()
	return s
}

func TestSnapshotPaginatesAndConservativelyIncludesOccupancy(t *testing.T) {
	d, p := testDomain()
	var s *httptest.Server
	s = localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ipam/vrfs/":
			if r.URL.Query().Get("offset") == "0" {
				w.Write(page([]any{map[string]any{"id": 7}}, s.URL+"/api/ipam/vrfs/?offset=200"))
				return
			}
			w.Write(page([]any{}, ""))
		case "/api/ipam/prefixes/":
			if r.URL.Query().Get("offset") == "0" {
				w.Write(page([]any{map[string]any{"id": 1, "prefix": "10.0.0.0/16", "vrf": map[string]any{"id": 7}}}, map[string]any{"not": "a URL"}))
				return
			}
			// This branch also proves that a malformed next value is rejected.
			w.Write(page([]any{}, ""))
		case "/api/ipam/ip-addresses/":
			w.Write(page([]any{map[string]any{"id": 4, "address": "10.0.1.10/24", "vrf": 7}}, ""))
		case "/api/ipam/ip-ranges/":
			w.Write(page([]any{map[string]any{"id": 5, "start_address": "10.0.2.1/32", "end_address": "10.0.2.3/32", "vrf": 7}}, ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	// The first prefix page above intentionally has an invalid next value;
	// the adapter must refuse to treat an incomplete page as a complete scan.
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(context.Background(), d); err == nil || !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("expected pagination error, got %v", err)
	}
}

// The inventory records no allocation scope, and the onboarding import needs
// one: it may write occupancy inside a managed VPC and must refuse inside a
// managed subnet (ADR 0010 as amended on 2026-09-20, internal/onboard's
// RuleInsideManagedVPC). platform_parent_allocation_id is what stands in for
// it, so the snapshot really carrying it through is load-bearing rather than
// incidental.
func TestSnapshotCarriesTheParentAllocationThatStandsInForScope(t *testing.T) {
	d, p := testDomain()
	prefixes := []any{
		map[string]any{"id": 1, "prefix": "10.0.0.0/16", "vrf": map[string]any{"id": 7},
			"custom_fields": map[string]any{"platform_pool_id": "pool"}},
		map[string]any{"id": 2, "prefix": "10.0.1.0/24", "vrf": map[string]any{"id": 7},
			"custom_fields": map[string]any{"platform_allocation_id": "alloc-vpc", "platform_parent_allocation_id": ""}},
		map[string]any{"id": 3, "prefix": "10.0.1.0/26", "vrf": map[string]any{"id": 7},
			"custom_fields": map[string]any{"platform_allocation_id": "alloc-subnet", "platform_parent_allocation_id": "alloc-vpc"}},
		map[string]any{"id": 4, "prefix": "10.0.2.0/24", "vrf": map[string]any{"id": 7},
			"tags":          []any{map[string]any{"slug": ImportedTag}},
			"custom_fields": map[string]any{"platform_import_batch": "batch-2026-09-20"}},
	}
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ipam/vrfs/":
			w.Write(page([]any{map[string]any{"id": 7}}, ""))
		case "/api/ipam/prefixes/":
			w.Write(page(prefixes, ""))
		case "/api/ipam/ip-addresses/", "/api/ipam/ip-ranges/":
			w.Write(page([]any{}, ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()

	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(context.Background(), d)
	if err != nil || !snap.Complete {
		t.Fatalf("snapshot: %#v %v", snap, err)
	}
	byCIDR := map[string]domain.Network{}
	for _, n := range snap.Networks {
		byCIDR[n.CIDR] = n
	}
	if got := byCIDR["10.0.1.0/24"]; got.AllocationID != "alloc-vpc" || got.ParentAllocationID != "" {
		t.Fatalf("the managed VPC: %#v", got)
	}
	if got := byCIDR["10.0.1.0/26"]; got.AllocationID != "alloc-subnet" || got.ParentAllocationID != "alloc-vpc" {
		t.Fatalf("the managed subnet: %#v", got)
	}
	// Occupancy and the pool container own nothing, so neither carries a
	// parent: "managed with no parent" has to mean a VPC and nothing else.
	if got := byCIDR["10.0.2.0/24"]; got.AllocationID != "" || got.ParentAllocationID != "" || !got.Imported {
		t.Fatalf("the imported occupancy: %#v", got)
	}
	if got := byCIDR["10.0.0.0/16"]; !got.ParentPool || got.AllocationID != "" || got.ParentAllocationID != "" {
		t.Fatalf("the pool container: %#v", got)
	}
}

func TestEnsureRecoversLostCreateResponse(t *testing.T) {
	d, p := testDomain()
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24", Request: domain.Request{AllocationKey: "key"}}
	created := false
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ipam/prefixes/" && r.Method == http.MethodPost {
			created = true
			// The write is durable but the response is lost.
			if h, ok := w.(http.Hijacker); ok {
				conn, _, _ := h.Hijack()
				_ = conn.Close()
				return
			}
		}
		if r.URL.Path == "/api/ipam/prefixes/" {
			if created {
				w.Write(page([]any{map[string]any{"id": 42, "prefix": a.CIDR, "vrf": 7, "custom_fields": map[string]any{allocationIDCF: a.ID, operationIDCF: "op-1"}}}, ""))
			} else {
				w.Write(page([]any{}, ""))
			}
			return
		}
		w.Write(page([]any{}, ""))
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Ensure(context.Background(), a, "op-1")
	if err != nil {
		t.Fatalf("lost response should recover: %v", err)
	}
	if id != "42" {
		t.Fatalf("got backend ID %q", id)
	}
}

func TestEnsureRejectsOperationMarkerConflict(t *testing.T) {
	d, p := testDomain()
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24"}
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(page([]any{map[string]any{"id": 9, "prefix": a.CIDR, "vrf": 7, "custom_fields": map[string]any{allocationIDCF: a.ID, operationIDCF: "other-op"}}}, ""))
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if _, err := c.Ensure(context.Background(), a, "op-1"); err == nil || !strings.Contains(err.Error(), "operation marker") {
		t.Fatalf("expected operation conflict, got %v", err)
	}
}

func TestSyncPreservesUnownedMetadata(t *testing.T) {
	d, p := testDomain()
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24", InventoryID: "42", State: domain.Active}
	var patched map[string]any
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "prefix": a.CIDR, "vrf": 7, "custom_fields": map[string]any{allocationIDCF: a.ID, operationIDCF: "op-1", "operator_note": "keep me"}})
			return
		}
		if r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&patched)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	fields, ok := patched["custom_fields"].(map[string]any)
	if !ok || fields["operator_note"] != "keep me" {
		t.Fatalf("unowned metadata was not preserved: %#v", patched)
	}
}

// Real NetBox renders an unset custom field as null. Found by running the
// onboarding import against a live NetBox: every fake in these tests simply
// omitted the key, so the "<nil>" string never appeared.
func TestStringCFTreatsNullAsUnset(t *testing.T) {
	fields := map[string]any{"platform_allocation_id": nil, "platform_pool_id": "pool_a", "n": 7}
	if got := stringCF(fields, "platform_allocation_id"); got != "" {
		t.Errorf("null custom field = %q, want empty", got)
	}
	if got := stringCF(fields, "missing"); got != "" {
		t.Errorf("missing custom field = %q, want empty", got)
	}
	if got := stringCF(fields, "platform_pool_id"); got != "pool_a" {
		t.Errorf("string custom field = %q", got)
	}
	if got := stringCF(fields, "n"); got != "7" {
		t.Errorf("numeric custom field = %q, want 7", got)
	}
}

// What an adoption is about to overwrite has to be in the snapshot, because the
// service records it in the ledger BEFORE the adapter is called (ADR 0012):
// the prefix's own status, and the account and region an import wrote.
func TestSnapshotCarriesWhatAnAdoptionWouldOverwrite(t *testing.T) {
	d, p := testDomain()
	prefixes := []any{
		map[string]any{"id": 1, "prefix": "10.0.0.0/16", "vrf": map[string]any{"id": 7},
			"custom_fields": map[string]any{"platform_pool_id": "pool"}},
		map[string]any{"id": 4, "prefix": "10.0.2.0/24", "vrf": map[string]any{"id": 7},
			"status": map[string]any{"value": "deprecated", "label": "Deprecated"},
			"tags":   []any{map[string]any{"slug": ImportedTag}},
			"custom_fields": map[string]any{"platform_import_batch": "batch-1",
				awsAccountCF: "123456789012", awsRegionCF: "eu-central-1"}},
	}
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ipam/vrfs/":
			w.Write(page([]any{map[string]any{"id": 7}}, ""))
		case "/api/ipam/prefixes/":
			w.Write(page(prefixes, ""))
		case "/api/ipam/ip-addresses/", "/api/ipam/ip-ranges/":
			w.Write(page([]any{}, ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(context.Background(), d)
	if err != nil || !snap.Complete {
		t.Fatalf("snapshot: %#v %v", snap, err)
	}
	for _, n := range snap.Networks {
		if n.CIDR != "10.0.2.0/24" {
			continue
		}
		if n.Status != "deprecated" || n.AWSAccountID != "123456789012" || n.AWSRegion != "eu-central-1" {
			t.Fatalf("the imported network: %#v", n)
		}
		return
	}
	t.Fatal("the imported network is not in the snapshot")
}
