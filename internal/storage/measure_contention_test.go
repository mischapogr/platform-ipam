package storage

// Opt-in, isolated SCALE-1 baseline. This measures the real PostgreSQL ledger
// lock with a synthetic state; it does not qualify customer or NetBox latency.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func TestMeasurePostgresLedgerContention(t *testing.T) {
	if os.Getenv("IPAM_MEASURE") != "1" {
		t.Skip("set IPAM_MEASURE=1 with IPAM_TEST_DATABASE_URL for isolated measurement")
	}
	db := requireIsolatedPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ledgers := make([]*PostgresLedger, 8)
	for i := range ledgers {
		ledgers[i] = db.openLedger()
	}
	if err := ledgers[0].Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seed := stateWithSizes(sizes{allocations: 1000, operations: 1000, idempotency: 1000, findings: 5, events: 10000})
	for id, op := range seed.Operations {
		op.Status = "DONE"
		seed.Operations[id] = op
	}
	if err := ledgers[0].Update(ctx, func(st *domain.State) error {
		st.Allocations, st.Operations, st.Requests = seed.Allocations, seed.Operations, seed.Requests
		st.Findings, st.Events = seed.Findings, seed.Events
		return nil
	}); err != nil {
		t.Fatalf("seed synthetic ledger: %v", err)
	}
	if err := ledgers[0].View(ctx, func(st *domain.State) error {
		if len(st.Allocations) != 1000 || len(st.Operations) != 1000 || len(st.Requests) != 1000 {
			return fmt.Errorf("incomplete seed: allocations=%d operations=%d requests=%d", len(st.Allocations), len(st.Operations), len(st.Requests))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	const perClient = 20
	for _, clients := range []int{1, 4, 8} {
		start := make(chan struct{})
		results := make(chan time.Duration, clients*perClient)
		errors := make(chan error, clients*perClient)
		var wg sync.WaitGroup
		begin := time.Now()
		for client := 0; client < clients; client++ {
			wg.Add(1)
			go func(client int) {
				defer wg.Done()
				<-start
				for i := 0; i < perClient; i++ {
					operationStart := time.Now()
					var err error
					if i%3 == 0 {
						id := fmt.Sprintf("alloc-%07d", client)
						err = ledgers[client].Update(ctx, func(st *domain.State) error {
							allocation, ok := st.Allocations[id]
							if !ok {
								return fmt.Errorf("missing %s", id)
							}
							allocation.UpdatedAt = time.Now().UTC()
							st.Allocations[id] = allocation
							return nil
						})
					} else {
						err = ledgers[client].View(ctx, func(st *domain.State) error {
							if len(st.Allocations) != 1000 {
								return fmt.Errorf("lost allocation during read")
							}
							return nil
						})
					}
					if err != nil {
						errors <- err
						return
					}
					results <- time.Since(operationStart)
				}
			}(client)
		}
		close(start)
		wg.Wait()
		close(results)
		close(errors)
		for err := range errors {
			t.Fatalf("%d-client measurement: %v", clients, err)
		}
		elapsed := time.Since(begin)
		var samples []time.Duration
		for sample := range results {
			samples = append(samples, sample)
		}
		if len(samples) != clients*perClient {
			t.Fatalf("%d-client measurement: got %d of %d samples", clients, len(samples), clients*perClient)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		p95Index := (95*len(samples)+99)/100 - 1
		t.Logf("SCALE1 clients=%d operations=%d updates=%d views=%d elapsed_ms=%d p50_ms=%d p95_ms=%d max_ms=%d ops_per_second=%.2f",
			clients, len(samples), clients*((perClient+2)/3), clients*(perClient-(perClient+2)/3), elapsed.Milliseconds(),
			samples[(len(samples)-1)/2].Milliseconds(), samples[p95Index].Milliseconds(),
			samples[len(samples)-1].Milliseconds(), float64(len(samples))/elapsed.Seconds())
		if err := ledgers[0].View(ctx, func(st *domain.State) error {
			for client := 0; client < clients; client++ {
				id := fmt.Sprintf("alloc-%07d", client)
				if !st.Allocations[id].UpdatedAt.After(seed.Allocations[id].UpdatedAt) {
					return fmt.Errorf("client %d update was lost", client)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
