package storage

import (
	"context"
	"encoding/json"
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
