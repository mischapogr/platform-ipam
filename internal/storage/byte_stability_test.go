package storage

// byte_stability_test.go proves, as a property over generated entities
// rather than by inspection, the claim persistState's diff (postgres.go)
// rests on: for every one of the five JSON-marshaled entity types this store
// persists (domain.Allocation, domain.Operation, domain.Idempotency,
// []domain.Observation, domain.Finding), marshalling an UNCHANGED decoded
// value a second time reproduces byte-identical output. If this did not
// hold, persistState's "did this row change" comparison could see two
// different byte strings for the same logical value and issue a redundant
// UPDATE for a row nothing actually changed -- a performance regression, per
// ADR 0017's own framing ("everything else must fail towards writing too
// much"), never a lost write: persistState's comparison only ever causes an
// EXTRA statement when it wrongly says "changed", never a missing one when
// it wrongly says "unchanged", because encoding/json is a pure function of
// the Go value (no randomisation, no external state) and the SAME process
// marshals both the loaded snapshot and the closure's result through the
// exact same code path. This file exists to show that failure mode does not
// arise in practice for any of the shapes this ledger stores, on three
// specific fronts named in package M4f's brief: map key order, time.Time
// round trips, and general struct round trips including every optional
// pointer/slice field each type carries.
//
// What it deliberately does NOT test: this is round-trip stability
// (Marshal(Unmarshal(Marshal(v))) == Marshal(v)), not "Marshal is injective"
// or "every possible Go value round-trips" -- neither claim is needed. The
// diff only ever compares two values that both passed through this exact
// Marshal/Unmarshal pipeline (the loaded snapshot was decoded from a
// database row, the closure's result started as a decoded value the closure
// may or may not have touched), so round-trip stability of THAT pipeline is
// the whole of what "did this row change" needs to be trustworthy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// roundTripStable asserts that marshalling v, decoding it back into a fresh
// zero value of the same type, and marshalling THAT reproduces byte-identical
// JSON. label identifies the case in a failure message.
func roundTripStable[T any](t *testing.T, label string, v T) {
	t.Helper()
	b1, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	var v2 T
	if err := json.Unmarshal(b1, &v2); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	b2, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("%s: remarshal: %v", label, err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("%s: marshal is not round-trip stable:\nfirst:  %s\nsecond: %s", label, b1, b2)
	}
}

// --- Generators. Every optional field (nil vs populated pointer, nil vs
// empty vs multi-entry slice/map, zero vs populated time) is exercised
// across the sweep below, not just once, because a stability failure is
// exactly the kind of thing that hides in an untouched code path. ---------

func stabilityGenTime(rng *rand.Rand) time.Time {
	// A mix of UTC and fixed-offset locations, and of second- and
	// nanosecond-granularity instants: RFC3339Nano (what encoding/json uses
	// for time.Time) has to survive both.
	base := time.Date(2020+rng.Intn(10), time.Month(1+rng.Intn(12)), 1+rng.Intn(28),
		rng.Intn(24), rng.Intn(60), rng.Intn(60), rng.Intn(1_000_000_000), time.UTC)
	switch rng.Intn(3) {
	case 0:
		return base
	case 1:
		return base.In(time.FixedZone("TEST+0530", 5*3600+30*60))
	default:
		return base.In(time.FixedZone("TEST-0700", -7*3600))
	}
}

func stabilityGenLabels(rng *rand.Rand, insertReversed bool) map[string]string {
	n := rng.Intn(5)
	if n == 0 {
		return map[string]string{}
	}
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("label-%02d-%c", i, 'a'+rune(rng.Intn(26)))
	}
	m := map[string]string{}
	// Insert in forward or reverse order: Go map iteration order is already
	// randomised per-process, so this is belt-and-braces, not the whole
	// proof -- encoding/json's own key-sort on Marshal is what actually
	// guarantees the output order, and that is what this whole file checks.
	order := keys
	if insertReversed {
		order = make([]string, n)
		for i, k := range keys {
			order[n-1-i] = k
		}
	}
	for i, k := range order {
		m[k] = fmt.Sprintf("value-%d-ü中", i) // non-ASCII, to catch an encoding difference too.
	}
	return m
}

func stabilityGenBinding(rng *rand.Rand) *domain.Binding {
	if rng.Intn(3) == 0 {
		return nil
	}
	b := &domain.Binding{
		Provider: "aws", ResourceType: "vpc", ResourceID: fmt.Sprintf("res-%d", rng.Int()),
		AccountID: "123456789012", Region: "eu-central-1",
	}
	if rng.Intn(2) == 0 {
		v := stabilityGenTime(rng)
		b.VerifiedAt = &v
	}
	return b
}

func stabilityGenTimePtr(rng *rand.Rand) *time.Time {
	if rng.Intn(2) == 0 {
		return nil
	}
	v := stabilityGenTime(rng)
	return &v
}

func stabilityGenAllocation(rng *rand.Rand, reversedMaps bool) domain.Allocation {
	blockers := []string(nil)
	switch rng.Intn(3) {
	case 1:
		blockers = []string{}
	case 2:
		blockers = []string{"quarantine_not_elapsed", "pending_binding"}
	}
	return domain.Allocation{
		Request: domain.Request{
			AllocationKey: fmt.Sprintf("key-%d", rng.Int()),
			Scope:         "vpc",
			Environment:   "prod",
			Region:        "eu-central-1",
			AccountID:     "123456789012",
			AddressFamily: "ipv4",
			PrefixLength:  24,
			Description:   "generated — with an em dash",
			Labels:        stabilityGenLabels(rng, reversedMaps),
		},
		ID:                 fmt.Sprintf("alloc_%d", rng.Int()),
		TenantID:           "tenant-a",
		DomainID:           "domain-a",
		RequestHash:        "hash",
		CIDR:               "10.0.0.0/24",
		PoolID:             "pool-a",
		State:              domain.Reserved,
		Revision:           rng.Int63(),
		PolicyVersion:      "v1",
		InventoryID:        "inv_1",
		InventorySync:      "CURRENT",
		Binding:            stabilityGenBinding(rng),
		CreatedAt:          stabilityGenTime(rng),
		UpdatedAt:          stabilityGenTime(rng),
		ReleaseRequestedAt: stabilityGenTimePtr(rng),
		QuarantineUntil:    stabilityGenTimePtr(rng),
		LastObservedAt:     stabilityGenTimePtr(rng),
		ReleaseBlockers:    blockers,
		Committed:          rng.Intn(2) == 0,
	}
}

func stabilityGenAPIError(rng *rand.Rand) *domain.APIError {
	if rng.Intn(2) == 0 {
		return nil
	}
	var details map[string]any
	if rng.Intn(2) == 0 {
		details = map[string]any{
			"reason":  "generated",
			"count":   rng.Intn(100),
			"nested":  map[string]any{"z": 1, "a": 2},
			"retries": []any{1, 2, 3},
		}
	}
	return &domain.APIError{Code: "some_code", Message: "generated message", Retryable: rng.Intn(2) == 0, Details: details}
}

func stabilityGenOperation(rng *rand.Rand, reversedMaps bool) domain.Operation {
	var adoption *domain.AdoptionRecord
	if rng.Intn(2) == 0 {
		adoption = &domain.AdoptionRecord{Operator: "ops:alice", NetworkID: "4242", ResourceID: "vpc-x", PriorStatus: "active", PriorAccountID: "123456789012", PriorRegion: "eu-central-1"}
	}
	return domain.Operation{
		ID: fmt.Sprintf("op_%d", rng.Int()), Type: "RESERVE", Status: "PENDING",
		AllocationID: "alloc_1", TenantID: "tenant-a", DomainID: "domain-a",
		Result:    stabilityGenLabels(rng, reversedMaps),
		Error:     stabilityGenAPIError(rng),
		Candidate: stabilityGenBinding(rng),
		Adoption:  adoption,
		CreatedAt: stabilityGenTime(rng), UpdatedAt: stabilityGenTime(rng),
	}
}

func stabilityGenIdempotency(rng *rand.Rand) domain.Idempotency {
	return domain.Idempotency{
		TenantID: "tenant-a", Method: "POST", Path: "POST /v1/allocations",
		Key: fmt.Sprintf("key-%d", rng.Int()), Hash: fmt.Sprintf("hash-%d", rng.Int()),
		AllocationID: "alloc_1", OperationID: "op_1",
	}
}

func stabilityGenResource(rng *rand.Rand, reversedMaps bool) domain.Resource {
	var cidrs []string
	switch rng.Intn(3) {
	case 1:
		cidrs = []string{}
	case 2:
		cidrs = []string{"10.0.0.0/24", "10.0.1.0/24"}
	}
	return domain.Resource{
		AccountID: "123456789012", Region: "eu-central-1", Type: "vpc",
		ID: fmt.Sprintf("res-%d", rng.Int()), CIDR: "10.0.0.0/16", CIDRs: cidrs,
		ParentID: "", ZoneID: "", Tags: stabilityGenLabels(rng, reversedMaps), State: "in-use",
	}
}

func stabilityGenObservations(rng *rand.Rand, reversedMaps bool) []domain.Observation {
	n := 1 + rng.Intn(3)
	out := make([]domain.Observation, n)
	for i := range out {
		resources := make([]domain.Resource, 1+rng.Intn(3))
		for j := range resources {
			resources[j] = stabilityGenResource(rng, reversedMaps)
		}
		out[i] = domain.Observation{
			DomainID: "domain-a", Generation: fmt.Sprintf("gen-%d", i), Complete: rng.Intn(2) == 0,
			StartedAt: stabilityGenTime(rng), FinishedAt: stabilityGenTime(rng), Resources: resources,
		}
		if rng.Intn(3) == 0 {
			out[i].Error = "generated error"
		}
	}
	return out
}

func stabilityGenFinding(rng *rand.Rand) domain.Finding {
	return domain.Finding{
		ID: fmt.Sprintf("finding_%d", rng.Int()), TenantID: "tenant-a", DomainID: "domain-a",
		AllocationID: "alloc_1", Code: "reservation_stuck", Severity: "CRITICAL", Status: "OPEN",
		AccountID: "123456789012", Region: "eu-central-1",
		ResourceType: "vpc", ResourceID: "res-1",
		FirstObservedAt: stabilityGenTime(rng), LastObservedAt: stabilityGenTime(rng),
	}
}

const stabilitySamples = 300

func TestByteStabilityAllocation(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("allocation[%d]", i), stabilityGenAllocation(rng, i%2 == 0))
	}
}

func TestByteStabilityOperation(t *testing.T) {
	rng := rand.New(rand.NewSource(20260923))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("operation[%d]", i), stabilityGenOperation(rng, i%2 == 0))
	}
}

func TestByteStabilityIdempotency(t *testing.T) {
	rng := rand.New(rand.NewSource(20260924))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("idempotency[%d]", i), stabilityGenIdempotency(rng))
	}
}

func TestByteStabilityObservations(t *testing.T) {
	rng := rand.New(rand.NewSource(20260925))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("observations[%d]", i), stabilityGenObservations(rng, i%2 == 0))
	}
}

func TestByteStabilityFinding(t *testing.T) {
	rng := rand.New(rand.NewSource(20260926))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("finding[%d]", i), stabilityGenFinding(rng))
	}
}

// TestByteStabilityMapKeyOrderIsIrrelevant is the map-key-order claim made
// explicit rather than left implicit in the samples above (which already
// alternate insertion order via reversedMaps, but a failure there would be
// easy to misread as "some other field differed"). Two maps with identical
// content inserted in opposite orders must marshal to byte-identical JSON --
// this is what lets persistState treat "the closure re-derived the same
// Labels map in a different iteration order" as unchanged rather than as a
// spurious diff.
func TestByteStabilityMapKeyOrderIsIrrelevant(t *testing.T) {
	forward := map[string]string{}
	backward := map[string]string{}
	keys := []string{"zeta", "delta", "alpha", "mu", "beta", "omega"}
	for i, k := range keys {
		forward[k] = fmt.Sprintf("v%d", i)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		backward[keys[i]] = fmt.Sprintf("v%d", i)
	}
	bf, err := json.Marshal(forward)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(backward)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bf, bb) {
		t.Fatalf("identical map content inserted in different orders marshalled differently:\nforward:  %s\nbackward: %s", bf, bb)
	}
}

// TestByteStabilityTimeRoundTrip is the time.Time claim made explicit: a
// value that already passed through one Marshal/Unmarshal cycle (which is
// what every "loaded" value in this store is, and what persistState is about
// to do to the closure's result too) must marshal identically the second
// time, across a spread of locations and sub-second precisions.
func TestByteStabilityTimeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20260927))
	for i := 0; i < stabilitySamples; i++ {
		roundTripStable(t, fmt.Sprintf("time[%d]", i), stabilityGenTime(rng))
	}
}
