package storage

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// MemoryLedger is a process-local ledger for unit tests and local examples.
// Update uses a copy-on-write transaction: a failed closure never changes the
// committed state and concurrent updates are serialized.
type MemoryLedger struct {
	mu    sync.RWMutex
	state *domain.State
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{state: domain.NewState()} }

func (l *MemoryLedger) View(ctx context.Context, fn func(*domain.State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := cloneState(l.state)
	return fn(s)
}

func (l *MemoryLedger) Update(ctx context.Context, fn func(*domain.State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := cloneState(l.state)
	if err := fn(s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.state = s
	return nil
}

func (l *MemoryLedger) Ready(context.Context) error { return nil }

// Events returns allocationID's full audit history, ordered exactly as
// PostgresLedger.Events orders it (At ascending, id as a tiebreaker) --
// "the memory ledger gives the same answers" (package M4f). Unlike the
// Postgres store, the memory ledger never stopped carrying every event in
// l.state.Events (View and Update here still clone the whole state on every
// call, package M4e's and M4f's read changes are Postgres-only), so this is
// a filter over what View already returns, not a separate code path with its
// own chance to disagree.
func (l *MemoryLedger) Events(ctx context.Context, allocationID string) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []domain.Event
	for _, e := range l.state.Events {
		if e.AllocationID == allocationID {
			out = append(out, e)
		}
	}
	sortEvents(out)
	return out, nil
}

// sortEvents is the one definition of "audit history order" both Ledger
// implementations use, so that a caller comparing the two (M4c's
// differential harness) never has to normalise an ordering difference that
// was really just two independent sort implementations agreeing by
// accident. At-ascending is the order a person reading "what happened to
// this allocation" actually wants; id, an opaque random token, is only a
// tiebreaker for two events an application clock recorded at the same
// instant.
func sortEvents(events []domain.Event) {
	sort.Slice(events, func(i, j int) bool {
		if !events[i].At.Equal(events[j].At) {
			return events[i].At.Before(events[j].At)
		}
		return events[i].ID < events[j].ID
	})
}

// Snapshot returns a detached copy, useful for deterministic tests.
func (l *MemoryLedger) Snapshot(ctx context.Context) (*domain.State, error) {
	var out *domain.State
	err := l.View(ctx, func(s *domain.State) error { out = s; return nil })
	return out, err
}

func cloneState(in *domain.State) *domain.State {
	if in == nil {
		return domain.NewState()
	}
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	out := domain.NewState()
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
	return out
}
