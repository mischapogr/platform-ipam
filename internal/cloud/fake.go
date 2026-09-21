package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// FakeConfig is the development/Compose observer configuration. A fixture is
// intentionally explicit: complete, domain_id, and generation must be
// present, so an omitted field cannot accidentally mean safe absence.
type FakeConfig struct {
	FixturePath string
	Resources   []domain.Resource
	DomainID    string
	Generation  string
	Complete    bool
	Unknown     bool
	Now         func() time.Time
	MaxAge      time.Duration
}

type Fake struct{ cfg FakeConfig }

func NewFake(cfg FakeConfig) *Fake {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 10 * time.Minute
	}
	return &Fake{cfg: cfg}
}

func (f *Fake) Observe(ctx context.Context, d domain.Domain) (domain.Observation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Observation{}, err
	}
	var obs domain.Observation
	if f.cfg.FixturePath != "" {
		data, err := os.ReadFile(f.cfg.FixturePath)
		if err != nil {
			return domain.Observation{}, err
		}
		if err := json.Unmarshal(data, &obs); err != nil {
			return domain.Observation{}, fmt.Errorf("decode fake AWS fixture: %w", err)
		}
		if obs.DomainID == "" || obs.Generation == "" {
			return domain.Observation{}, errors.New("fake fixture must explicitly set domain_id and generation")
		}
		if !obs.Complete && !hasCompleteField(data) {
			return domain.Observation{}, errors.New("fake fixture must explicitly set complete")
		}
		if obs.DomainID != d.ID {
			return domain.Observation{}, fmt.Errorf("fake fixture domain %q does not match %q", obs.DomainID, d.ID)
		}
		if obs.Generation != d.CoverageGeneration {
			return domain.Observation{}, fmt.Errorf("fake fixture generation %q does not match %q", obs.Generation, d.CoverageGeneration)
		}
		if len(f.cfg.Resources) > 0 {
			obs.Resources = append([]domain.Resource(nil), f.cfg.Resources...)
		}
		if obs.Complete && (obs.FinishedAt.IsZero() || obs.StartedAt.IsZero()) {
			now := f.cfg.Now().UTC()
			obs.StartedAt = now
			obs.FinishedAt = now
		}
	} else {
		if f.cfg.DomainID != "" && f.cfg.DomainID != d.ID {
			return InventoryObservationError(d.ID, d.CoverageGeneration, "fake domain mismatch")
		}
		if f.cfg.Generation != "" && f.cfg.Generation != d.CoverageGeneration {
			return InventoryObservationError(d.ID, d.CoverageGeneration, "fake generation mismatch")
		}
		obs = domain.Observation{DomainID: d.ID, Generation: d.CoverageGeneration, Complete: f.cfg.Complete && !f.cfg.Unknown, Resources: append([]domain.Resource(nil), f.cfg.Resources...)}
		now := f.cfg.Now().UTC()
		obs.StartedAt = now
		obs.FinishedAt = now
	}
	if !obs.Complete {
		if obs.Error == "" {
			obs.Error = "fake observation is UNKNOWN"
		}
		return obs, errors.New(obs.Error)
	}
	if obs.FinishedAt.After(f.cfg.Now().Add(time.Minute)) {
		obs.Complete = false
		obs.Error = "fake fixture timestamp is in the future"
		return obs, errors.New(obs.Error)
	}
	if f.cfg.MaxAge > 0 && f.cfg.Now().Sub(obs.FinishedAt) > f.cfg.MaxAge {
		obs.Complete = false
		obs.Error = "fake observation is stale"
		return obs, errors.New(obs.Error)
	}
	obs.Complete = true
	return obs, nil
}

func hasCompleteField(data []byte) bool {
	var raw map[string]json.RawMessage
	return json.Unmarshal(data, &raw) == nil && raw["complete"] != nil
}

// InventoryObservationError is a small helper for fake configuration errors;
// it keeps returned observations in the same UNKNOWN shape as runtime failures.
func InventoryObservationError(domainID, generation, msg string) (domain.Observation, error) {
	return domain.Observation{DomainID: domainID, Generation: generation, Complete: false, Error: msg}, errors.New(msg)
}

var _ domain.Observer = (*Fake)(nil)
