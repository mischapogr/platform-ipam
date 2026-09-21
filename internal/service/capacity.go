package service

import (
	"context"
	"net/netip"
	"sort"
	"strconv"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

type blockInterval struct{ first, last uint64 }

// Counts aligned blocks touched by occupied prefixes. Interval unions avoid
// enumerating millions of candidate CIDRs or double counting nested networks.
func occupiedBlocks(base netip.Prefix, bits int, prefixes []netip.Prefix) uint64 {
	start := addrInt(base.Addr())
	end := start + (uint64(1) << uint(32-base.Bits())) - 1
	var ranges []blockInterval
	for _, p := range prefixes {
		if !p.Addr().Is4() || !base.Overlaps(p) {
			continue
		}
		lo := addrInt(p.Masked().Addr())
		hi := lo + (uint64(1) << uint(32-p.Bits())) - 1
		if lo < start {
			lo = start
		}
		if hi > end {
			hi = end
		}
		ranges = append(ranges, blockInterval{(lo - start) >> uint(32-bits), (hi - start) >> uint(32-bits)})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].first < ranges[j].first })
	var total uint64
	for i := 0; i < len(ranges); {
		r := ranges[i]
		i++
		for i < len(ranges) && ranges[i].first <= r.last+1 {
			if ranges[i].last > r.last {
				r.last = ranges[i].last
			}
			i++
		}
		total += r.last - r.first + 1
	}
	return total
}

func (s *Service) capacity(ctx context.Context, p domain.Principal, poolID string) (map[string]any, error) {
	var pool *domain.Pool
	for i := range s.cfg.Pools {
		candidate := &s.cfg.Pools[i]
		if candidate.ID != poolID {
			continue
		}
		// ADR 0011 stage two: an operator has no tenant, environment, region or
		// account to be eligible with, and asks what a pool holds rather than
		// what it may reserve from it, so the pool is selected by id alone. An id
		// that names no configured pool is still 404 for an operator.
		if p.IsOperator() {
			pool = candidate
			continue
		}
		if eligibleString(candidate.EligibleTenants, p.TenantID) && eligibleString(p.Environments, candidate.Environment) && eligibleString(p.Regions, candidate.Region) {
			for _, account := range p.Accounts {
				if eligibleString(candidate.EligibleAccounts, account) {
					pool = candidate
					break
				}
			}
		}
	}
	if pool == nil {
		return nil, apiErr(404, "not_found", "pool not found")
	}
	d := domainFor(s.cfg, pool.DomainID)
	base, err := netip.ParsePrefix(pool.CIDR)
	if err != nil {
		return nil, apiErr(503, "dependency_unavailable", "pool configuration is unavailable")
	}
	now := s.now().UTC()
	observedAt := now
	complete := false
	var snapshot domain.InventorySnapshot
	if s.inventory != nil {
		snapshot, err = s.inventory.Snapshot(ctx, d)
		complete = err == nil && snapshot.Complete
	}
	occupied := []netip.Prefix{}
	categories := map[string][]netip.Prefix{"reserved": {}, "active": {}, "quarantined": {}, "pending": {}, "excluded": {}}
	add := func(raw string, category string) {
		q, e := netip.ParsePrefix(raw)
		if e != nil || !q.Addr().Is4() {
			complete = false
			return
		}
		occupied = append(occupied, q)
		if category != "" {
			categories[category] = append(categories[category], q)
		}
	}
	for _, raw := range pool.ExcludedCIDRs {
		add(raw, "excluded")
	}
	for _, n := range snapshot.Networks {
		if !n.ParentPool {
			add(n.CIDR, "")
		}
	}
	err = s.ledger.View(ctx, func(st *domain.State) error {
		rows := st.Observations[d.ID]
		if len(rows) == 0 {
			complete = false
		} else {
			o := rows[len(rows)-1]
			if !o.FinishedAt.IsZero() {
				observedAt = o.FinishedAt
			}
			if !trustedObservation(o, d, now, s.cfg.Lifecycle.MaxObservationAge) {
				complete = false
			}
			for _, r := range o.Resources {
				add(r.CIDR, "")
				for _, raw := range r.CIDRs {
					add(raw, "")
				}
			}
		}
		if pendingDomain(st, d.ID) != nil {
			complete = false
		}
		for _, a := range st.Allocations {
			if a.DomainID != d.ID || a.State == domain.Released {
				continue
			}
			category := ""
			// ADR 0011 stage two: the occupied set this loop feeds through add()
			// -- and therefore `allocatable` below -- is already domain-wide and
			// is the same number for everybody; only this lifecycle breakdown is
			// tenant-scoped, by the single conjunct dropped here. An operator
			// sees the breakdown over every tenant's vpc allocations in the pool.
			if (p.IsOperator() || a.TenantID == p.TenantID) && a.Scope == "vpc" {
				if !a.Committed {
					category = "pending"
				} else {
					switch a.State {
					case domain.Reserved:
						category = "reserved"
					case domain.Active:
						category = "active"
					case domain.Quarantined:
						category = "quarantined"
					}
				}
			}
			add(a.CIDR, category)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	bySize := map[string]any{}
	for _, bits := range pool.AllowedPrefixLengths {
		if bits < base.Bits() || bits > 32 {
			continue
		}
		counts := map[string]uint64{}
		for category, prefixes := range categories {
			counts[category] = occupiedBlocks(base, bits, prefixes)
		}
		counts["allocatable"] = 0
		if complete {
			total := uint64(1) << uint(bits-base.Bits())
			counts["allocatable"] = total - occupiedBlocks(base, bits, occupied)
		}
		bySize[strconv.Itoa(bits)] = counts
	}
	return map[string]any{"pool_id": pool.ID, "observed_at": observedAt, "complete": complete, "by_prefix_length": bySize}, nil
}
