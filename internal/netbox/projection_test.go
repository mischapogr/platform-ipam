package netbox

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func TestSyncSkipsUnchangedProjectionAndRefreshesNewObservationOnCadence(t *testing.T) {
	d, p := testDomain()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	older := now.Add(-5 * time.Minute)
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24", InventoryID: "42", State: domain.Active, LastObservedAt: &now}
	fields := ownedFields(a, "op-1")
	fields["platform_last_observed_at"] = older.Format(time.RFC3339)
	fields[parentAllocationIDCF] = nil // NetBox renders unset text fields as null.
	fields["operator_note"] = "keep me"
	status := "active"
	gets, patches := 0, 0
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "prefix": a.CIDR, "vrf": 7, "status": status, "custom_fields": fields})
		case http.MethodPatch:
			patches++
			var body struct {
				Status string         `json:"status"`
				Fields map[string]any `json:"custom_fields"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch: %v", err)
			}
			status, fields = body.Status, body.Fields
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}, ProjectionRefreshInterval: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if gets != 1 || patches != 0 {
		t.Fatalf("unchanged projection: GET=%d PATCH=%d, want 1/0", gets, patches)
	}

	// A lifecycle change is published immediately, while the observation stamp
	// retains the older value until its separate refresh cadence is due.
	a.State = domain.Reserved
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if patches != 1 || status != "reserved" || fields["platform_last_observed_at"] != older.Format(time.RFC3339) || fields["operator_note"] != "keep me" {
		t.Fatalf("immediate lifecycle patch lost status, stamp, or unowned metadata: patches=%d status=%q fields=%v", patches, status, fields)
	}

	c.now = func() time.Time { return now.Add(5 * time.Minute) }
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if gets != 3 || patches != 2 || fields["platform_last_observed_at"] != now.Format(time.RFC3339) {
		t.Fatalf("due observation refresh: GET=%d PATCH=%d stamp=%v", gets, patches, fields["platform_last_observed_at"])
	}
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if gets != 4 || patches != 2 {
		t.Fatalf("replayed observation should be GET-only: GET=%d PATCH=%d", gets, patches)
	}
}

func TestSyncNeverRegressesObservationOrSkipsIdentityFailure(t *testing.T) {
	d, p := testDomain()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Hour)
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24", InventoryID: "42", State: domain.Active, LastObservedAt: &older}
	fields := ownedFields(a, "op-1")
	fields["platform_last_observed_at"] = now.Format(time.RFC3339)
	patches := 0
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "prefix": a.CIDR, "vrf": 7, "status": "active", "custom_fields": fields})
			return
		}
		patches++
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}, ProjectionRefreshInterval: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now.Add(time.Hour) }
	if err := c.Sync(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatal("older ledger observation regressed the NetBox stamp")
	}
	fields[allocationIDCF] = "other-allocation"
	if err := c.Sync(context.Background(), a); err == nil || patches != 0 {
		t.Fatalf("identity mismatch was skipped: err=%v PATCH=%d", err, patches)
	}
}
