package cloud

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func TestFakeValidatesDomainGenerationAndReturnsFreshTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cloud.json")
	if err := os.WriteFile(path, []byte(`{"domain_id":"d","generation":"g","complete":true,"resources":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	f := NewFake(FakeConfig{FixturePath: path, Now: func() time.Time { return now }})
	obs, err := f.Observe(context.Background(), domain.Domain{ID: "d", CoverageGeneration: "g"})
	if err != nil || !obs.Complete || !obs.FinishedAt.Equal(now) {
		t.Fatalf("valid fixture was not fresh/complete: %#v err=%v", obs, err)
	}
	obs, err = f.Observe(context.Background(), domain.Domain{ID: "d", CoverageGeneration: "changed"})
	if err == nil || obs.Complete {
		t.Fatalf("generation mismatch must be UNKNOWN: %#v err=%v", obs, err)
	}
}

func TestFakeUnknownFixtureAndInjectedResources(t *testing.T) {
	f := NewFake(FakeConfig{DomainID: "d", Generation: "g", Resources: []domain.Resource{{ID: "vpc-1", Type: "vpc", CIDR: "10.0.0.0/16"}}, Unknown: true})
	obs, err := f.Observe(context.Background(), domain.Domain{ID: "d", CoverageGeneration: "g"})
	if err == nil || obs.Complete || len(obs.Resources) != 1 {
		t.Fatalf("unknown fake observation lost state: %#v err=%v", obs, err)
	}
}
