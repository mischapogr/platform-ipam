package netbox

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// What the adapter tells the service about an error is one bit: did it get an
// answer about the object or not. A 5xx or a 429 is no answer -- the service
// retries in silence; a 4xx is NetBox reading the request and refusing it --
// repeating it changes nothing, so a person must be told. Both adoption and its
// abandon classify the same way, through the same function.
func TestOnlyAnUnansweredRequestIsMarkedUncertain(t *testing.T) {
	cases := map[string]struct {
		status    int
		uncertain bool
	}{
		"503 service unavailable": {http.StatusServiceUnavailable, true},
		"500 internal error":      {http.StatusInternalServerError, true},
		"429 too many requests":   {http.StatusTooManyRequests, true},
		"403 forbidden":           {http.StatusForbidden, false},
		"400 bad request":         {http.StatusBadRequest, false},
	}
	for name, tc := range cases {
		t.Run("adopt/"+name, func(t *testing.T) {
			stack := &adoptStack{etag: `W/"1"`, patchStatus: tc.status,
				prefixes: map[int]map[string]any{42: importedPrefix(42, "10.0.1.0/24", 7)}}
			c, closer := adoptClient(t, stack)
			defer closer()
			_, err := c.Adopt(context.Background(), adoptAllocation(), "op-1")
			if err == nil {
				t.Fatal("a failed write reported success")
			}
			if got := errors.Is(err, domain.ErrInventoryUncertain); got != tc.uncertain {
				t.Fatalf("uncertain=%v, want %v: %v", got, tc.uncertain, err)
			}
		})
		t.Run("abandon/"+name, func(t *testing.T) {
			stack, a := abandonStack(t)
			stack.patchStatus = tc.status
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.AbandonAdoption(context.Background(), a, "op-1", domain.PriorInventory{})
			if !errors.Is(err, ErrAbandonUncertain) {
				t.Fatalf("got %v, want the adapter's own %v", err, ErrAbandonUncertain)
			}
			if got := errors.Is(err, domain.ErrInventoryUncertain); got != tc.uncertain {
				t.Fatalf("uncertain=%v, want %v: %v", got, tc.uncertain, err)
			}
		})
		// Package H8a: the cancel is the one port operation that destroys an
		// object, so whether its failure was an answer decides between asking
		// a consumer to try again and telling a person that their hold cannot
		// be withdrawn. It classifies through the same unanswered function.
		t.Run("cancel/"+name, func(t *testing.T) {
			stack, a := cancelStack()
			stack.deleteStatus = tc.status
			c, closer := adoptClient(t, stack)
			defer closer()
			err := c.CancelReservation(context.Background(), a, "op-1")
			if !errors.Is(err, ErrCancelUncertain) {
				t.Fatalf("got %v, want the adapter's own %v", err, ErrCancelUncertain)
			}
			if got := errors.Is(err, domain.ErrInventoryUncertain); got != tc.uncertain {
				t.Fatalf("uncertain=%v, want %v: %v", got, tc.uncertain, err)
			}
			// Whatever the status, a delete that was refused left the object
			// alone, so nothing may look as if it had been removed.
			if _, present := stack.prefixes[42]; !present {
				t.Fatalf("a delete answered %d still removed the prefix", tc.status)
			}
		})
		// Package H4: Ensure did not mark its own request errors this way,
		// so a NetBox 5xx or a timeout answered a worker's reservation
		// recovery exactly like a definite refusal (internal/netbox/
		// client.go's uncertainEnsure, added for this package).
		t.Run("ensure/"+name, func(t *testing.T) {
			d, p := testDomain()
			s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/ipam/prefixes/" {
					w.WriteHeader(tc.status)
					return
				}
				http.NotFound(w, r)
			}))
			defer s.Close()
			c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
			if err != nil {
				t.Fatal(err)
			}
			a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24"}
			if _, err := c.Ensure(context.Background(), a, "op-1"); err == nil {
				t.Fatal("a failed list reported success")
			} else if got := errors.Is(err, domain.ErrInventoryUncertain); got != tc.uncertain {
				t.Fatalf("uncertain=%v, want %v: %v", got, tc.uncertain, err)
			}
		})
	}
}

// A definite refusal Ensure reaches on evidence it read -- the CIDR is
// already occupied by an unrelated prefix in the managed VRF -- must never
// be marked uncertain, or a worker's reservation recovery would retry it
// forever instead of raising reservation_stuck (internal/service/worker.go's
// recoverReservations, package H4).
func TestEnsureCIDRAlreadyOccupiedIsNotUncertain(t *testing.T) {
	d, p := testDomain()
	s := localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/ipam/prefixes/" {
			if r.URL.Query().Get("prefix") == "10.0.1.0/24" {
				w.Write(page([]any{map[string]any{"id": 99, "prefix": "10.0.1.0/24", "vrf": map[string]any{"id": 7}}}, ""))
				return
			}
			w.Write(page([]any{}, ""))
			return
		}
		http.NotFound(w, r)
	}))
	defer s.Close()
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	a := domain.Allocation{ID: "alloc-1", DomainID: d.ID, PoolID: p.ID, CIDR: "10.0.1.0/24"}
	_, err = c.Ensure(context.Background(), a, "op-1")
	if err == nil || !strings.Contains(err.Error(), "already occupied") {
		t.Fatalf("want a definite occupied refusal, got %v", err)
	}
	if errors.Is(err, domain.ErrInventoryUncertain) {
		t.Fatalf("a definite CIDR-occupied refusal must not be uncertain: %v", err)
	}
}
