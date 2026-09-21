package onboard

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// maxOnboardRangeBlocks mirrors internal/netbox/client.go's maxRangeBlocks:
// once rangeCIDRs there would need more than this many CIDR blocks to cover a
// start/end range, it stops and substitutes one covering (over-blocking)
// prefix instead. Duplicated here, as a constant only, because this package
// must stay free of internal/netbox (design section 2: the validator is a
// pure function; only the adapter writes to NetBox). For a 32-bit address
// space the greedy largest-aligned-block algorithm below can never actually
// need more than roughly 2*32 blocks (a well-known bound for this class of
// range-to-prefix decomposition), so on real IPv4 input this rule is a
// defensive parity check with the adapter's cap rather than one expected to
// fire; see plan_test.go's TestPlanRangeTooLarge for how the mechanism itself
// is exercised.
const maxOnboardRangeBlocks = 4096

// accountIDPattern mirrors internal/config/config.go's accountPattern.
// Duplicated rather than imported so this package does not reach into
// internal/config's decode-and-validate-a-file concerns for one line of
// regex.
var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// Rule identifiers. Stable across releases: callers (C5's onboard command,
// and any future consumer of a Report) may switch on these.
const (
	// RuleNormalize carries an error/warning/info Diagnostic produced while
	// parsing or normalizing the table (package C1) into the plan unchanged:
	// an error diagnostic from normalization is an error in the plan too.
	RuleNormalize = "normalize"

	// RuleDuplicateCIDR: two or more rows resolve to the same CIDR, or the
	// same start/end range (design section 5, "two rows with the same
	// CIDR" -- same VPC range in two accounts, or a subnet equal to its
	// VPC). They collapse to one write listing every source row; warning,
	// because internal/netbox/client.go's Snapshot fails the ENTIRE
	// inventory snapshot on a duplicate (vrf, cidr) --
	// `return domain.InventorySnapshot{}, fmt.Errorf("duplicate prefix %s in VRF %d", cidr, p.vrfID())`
	// -- which would turn every reservation in the domain into a 503.
	RuleDuplicateCIDR = "duplicate-cidr"

	// RulePoolExactMatch: the row's CIDR equals a configured pool's own
	// CIDR. internal/netbox/client.go's Snapshot requires the pool CIDR to
	// match exactly one prefix ("configured pool %s has %d matching
	// prefixes"); importing a second, unmanaged prefix at that exact CIDR
	// would make that check fail and take the domain's snapshot down.
	RulePoolExactMatch = "pool-exact-match"

	// RulePoolAncestor: the row's CIDR contains a configured pool as a
	// subnet. internal/netbox/client.go's Snapshot silently drops such a
	// prefix as an "ancestor" of the pool --
	//   pool := false; ancestor := false
	//   for _, configured := range pools {
	//       ...
	//       if pc.Bits() > cidr.Bits() && cidr.Contains(pc.Addr()) { ancestor = true }
	//   }
	//   if ancestor && !pool { continue }
	// -- so it would never appear in the snapshot at all and would block
	// nothing, while the pool's address space is in fact already occupied
	// by whatever this row describes.
	RulePoolAncestor = "pool-ancestor"

	// RuleRangeSpansPool is RulePoolAncestor's range equivalent: a
	// start/end range that spans (or exactly bounds) a configured pool.
	// Snapshot has no ancestor rule for ranges -- rangeCIDRs turns the
	// whole span into covering CIDR blocks -- so importing such a range
	// would mark the entire pool as occupied and take the domain's
	// allocation offline. Mirrors internal/netbox/occupancy.go's
	// refusePoolSpan.
	RuleRangeSpansPool = "range-spans-pool"

	// RuleOverlapsManaged: the row overlaps a snapshot Network that already
	// carries a non-empty AllocationID -- platform-managed space that was
	// in use before the import. Needs a human, not an import. The one
	// exception is RuleInsideManagedVPC below.
	RuleOverlapsManaged = "overlaps-managed"

	// RuleInsideManagedVPC: the row lies strictly inside a platform-managed
	// **VPC** allocation's prefix, so it is importable as unmanaged
	// occupancy rather than refused. Informational, and the row is written.
	//
	// It exists because a VPC can be adopted (ADR 0010) before all of its
	// subnets exist, and a subnet created afterwards could otherwise never be
	// imported: RuleOverlapsManaged refused every overlap with managed space
	// in either direction, so "import everything, then adopt" was the only
	// order that ever worked, and an estate that keeps growing has no such
	// order. A subnet inside an adopted VPC is exactly the occupancy ADR 0007
	// describes -- it blocks address space and owns nothing -- and it is what
	// a later `adopt` of that subnet has to find in the inventory.
	//
	// Everything else is unchanged: a row equal to a managed network, a row
	// containing one, a row only partly overlapping one, and any row
	// overlapping a managed **subnet** are all still RuleOverlapsManaged
	// errors. onboard still never opens the ledger; the scope comes from the
	// snapshot alone (domain.Network.ParentAllocationID).
	RuleInsideManagedVPC = "inside-managed-vpc"

	// RuleAlreadyUnmanaged: the row's CIDR is already present in the
	// snapshot as unmanaged occupancy (no AllocationID, not a pool's own
	// prefix). Informational, and the entry is skipped from the write set
	// so a re-run of plan/apply over the same table is idempotent.
	RuleAlreadyUnmanaged = "already-unmanaged"

	// RuleOutsidePool: the row lies outside every configured pool in this
	// domain. Harmless, but useful for an operator to see, so it is
	// reported and still written.
	RuleOutsidePool = "outside-pool"

	// RuleRangeTooLarge: the start/end range would decompose into more
	// than maxOnboardRangeBlocks CIDR blocks. See the constant's comment.
	RuleRangeTooLarge = "range-too-large"

	// RuleInvalidAccountID and RuleInvalidRoleARN mirror
	// internal/config/config.go's account/role validation for an accounts
	// table row.
	RuleInvalidAccountID = "invalid-account-id"
	RuleInvalidRoleARN   = "invalid-role-arn"

	// RuleUnknownDomain and RuleDomainNoVRF are table-level findings (Rows
	// is empty): plan cannot classify any row without a real domain and a
	// configured VRF to read occupancy from.
	RuleUnknownDomain = "unknown-domain"
	RuleDomainNoVRF   = "domain-no-vrf"
	// RuleIncompleteSnapshot refuses to plan against an inventory snapshot
	// that is not complete. An empty Networks list from a failed or partial
	// read looks exactly like an empty VRF, and every "already present" and
	// "overlaps managed" check would then pass vacuously. The service applies
	// the same rule to itself: Reserve answers 503 without a complete
	// snapshot rather than allocate against one (internal/service/service.go).
	RuleIncompleteSnapshot = "incomplete-snapshot"

	// RuleInvalidCIDR is defence in depth: a non-canonical CIDR or an IPv6
	// value should already have been turned into an error or a
	// warning-and-skip by package C1's normalizer, but Plan does not trust
	// that and re-validates every CIDR, start and end address it is given.
	RuleInvalidCIDR = "invalid-cidr"

	// Contributor findings (ADR 0016, package M9b2, "the comparison,
	// read-only"). Each is raised only when a networks-table row's CIDR
	// matches a network the snapshot already carries as unmanaged occupancy
	// (the same condition RuleAlreadyUnmanaged tests): with no existing
	// prefix there is nothing to compare a table's own contributors against,
	// so a brand-new import raises none of these. Nothing here ever writes;
	// that is package M9b3's `apply --refresh`.

	// RuleContributorNew: this table names a contributor the prefix does not
	// carry yet. Info. This is the only one of the five that a later refresh
	// (M9b3) writes on. Never raised for a degenerate identity (see
	// contributorFindings in this file): a row with no resource_id collapses
	// several different VPCs onto one identity string, so it can never be
	// proven new against a prefix that already carries that same string.
	RuleContributorNew = "contributor-new"

	// RuleContributorAbsent: the prefix carries a contributor this table
	// does not name. Warning, and NEVER a write, however apply is called: an
	// import table is a scope, not a census -- ADR 0016's own words -- so
	// absence from one table proves nothing about the estate. Never raised
	// for a degenerate identity, for the same reason RuleContributorNew
	// never is: a table lacking a resource_id column could still be naming
	// the same resource under the same collapsed identity, so "gone" cannot
	// be proved either.
	RuleContributorAbsent = "contributor-absent"

	// RuleContributorUnknown: the prefix carries no contributor list at all,
	// because it was imported before ADR 0016 landed (package M9b1).
	// Info. A refresh may populate it, and the prefix then carries a flag
	// (ADR 0016) recording that the list was reconstructed rather than
	// written by the import that first created the prefix -- the two cannot
	// be told apart afterwards. Mutually exclusive with every other
	// contributor finding for this CIDR: there is nothing else to compare.
	RuleContributorUnknown = "contributor-unknown"

	// RuleContributorStaleSource: a contributor entry this table would write
	// has a null observed_at (no observed_at column in its source file, or a
	// present-but-blank cell -- internal/onboard.NetworkRow's
	// ObservedAtColumnPresent tells the two apart and both are null here).
	// Info, and the flag that makes the entry unusable as removal evidence
	// (ADR 0016). Raised per table row, independent of whether that row's
	// identity is new or already present: the row's own provenance is what
	// is being reported, not a comparison outcome.
	RuleContributorStaleSource = "contributor-stale-source"

	// RuleContributorUnreadable: the prefix's platform_import_contributors
	// field holds something this version cannot decode as a contributor list
	// (domain.Network.ContributorsUnreadable) -- an operator's hand edit, or
	// a shape a later version wrote. ADR 0016's package M9b2 block is
	// explicit that this is "its own case (not contributor-unknown, never
	// reconstructed silently)"; the ADR itself does not name a rule for it,
	// so this identifier and its WARNING level are this package's own
	// decision (see the M9b2 report's "decisions the record did not make").
	// Warning, not info: an unreadable field is a data-quality problem an
	// operator can act on, unlike a merely absent one. No other contributor
	// finding is computed alongside it for the same CIDR -- an unreadable
	// list cannot be compared against, so nothing here treats its contents
	// as known enough to say "new" or "absent" about.
	RuleContributorUnreadable = "contributor-unreadable"
)

// WriteKind identifies what apply would write for one WriteEntry.
type WriteKind string

const (
	WritePrefix WriteKind = "prefix"
	WriteRange  WriteKind = "range"
)

// WriteEntry is one CIDR or address range apply would write to NetBox
// occupancy (internal/netbox/occupancy.go's EnsureOccupancy), after
// duplicate rows have been collapsed. SourceRows is sorted and never empty.
type WriteEntry struct {
	Kind         WriteKind
	CIDR         string // set when Kind == WritePrefix
	StartAddress string // set when Kind == WriteRange
	EndAddress   string // set when Kind == WriteRange
	SourceRows   []int
}

// Finding is one condition Plan raised while classifying the table. Rule is
// a stable identifier (see the Rule* constants); Rows is 1-based and may be
// empty for a table-level finding.
type Finding struct {
	Level   Level
	Rule    string
	Rows    []int
	CIDR    string
	Message string
}

// sortKey orders findings deterministically: empty-CIDR (table-level, or an
// account/role) findings first, then by CIDR, then by rule, then by the
// first source row, then by message as a final tiebreak.
func (f Finding) sortKey() (string, string, int, string) {
	row := 0
	if len(f.Rows) > 0 {
		row = f.Rows[0]
	}
	return f.CIDR, f.Rule, row, f.Message
}

// Report is the result of Plan: the surviving write set apply would act on
// (design section 6), plus every Finding raised classifying the table
// (design section 5, plus package C1's Diagnostics carried through under
// RuleNormalize).
type Report struct {
	Writes   []WriteEntry
	Findings []Finding
}

// HasErrors reports whether apply would be refused for this table.
func (r Report) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

// Plan validates a normalized Table against the real pools configuration and
// a NetBox inventory snapshot, and reports what apply would write and every
// condition it found (docs/ONBOARDING_IMPORT.md section 5). Plan is a pure
// function: it does no I/O, calls no NetBox, and loads no configuration --
// cfg and snap are passed in by the caller (package C5's onboard command).
func Plan(table Table, cfg domain.Config, domainID string, snap domain.InventorySnapshot) Report {
	var findings []Finding
	for _, d := range table.Diagnostics {
		f := Finding{Level: d.Level, Rule: RuleNormalize, Message: d.Message}
		if d.Row != 0 {
			f.Rows = []int{d.Row}
		}
		if d.Column != "" {
			f.Message = fmt.Sprintf("%s: %s", d.Column, f.Message)
		}
		findings = append(findings, f)
	}

	d, ok := findDomain(cfg, domainID)
	if !ok {
		findings = append(findings, Finding{Level: LevelError, Rule: RuleUnknownDomain,
			Message: fmt.Sprintf("unknown domain %q", domainID)})
		return finishReport(nil, findings)
	}
	if d.Backend.VRFID == 0 {
		findings = append(findings, Finding{Level: LevelError, Rule: RuleDomainNoVRF,
			Message: fmt.Sprintf("domain %q has no VRF configured", domainID)})
		return finishReport(nil, findings)
	}
	if !snap.Complete {
		findings = append(findings, Finding{Level: LevelError, Rule: RuleIncompleteSnapshot,
			Message: "the inventory snapshot is not complete; an unreadable inventory is not an empty one"})
		return finishReport(nil, findings)
	}

	var pools []domain.Pool
	for _, p := range cfg.Pools {
		if p.DomainID == d.ID {
			pools = append(pools, p)
		}
	}

	var writes []WriteEntry
	switch table.Kind {
	case KindAccounts:
		findings = append(findings, planAccounts(table.Accounts)...)
	case KindNetworks:
		w, f := planPrefixCandidates(networkCandidates(table.Networks), pools, snap, table.Networks)
		writes = append(writes, w...)
		findings = append(findings, f...)
	case KindRanges:
		var cidrRows, rangeRows []RangeRow
		for _, r := range table.Ranges {
			if r.CIDR != "" {
				cidrRows = append(cidrRows, r)
			} else {
				rangeRows = append(rangeRows, r)
			}
		}
		// nil networkRows: a ranges table's CIDR rows never carry
		// contributors (internal/onboardcmd's entryOccupancy never builds
		// them for a KindRanges write either), so no contributor comparison
		// applies here -- only the plain duplicate-CIDR message.
		w, f := planPrefixCandidates(rangeCIDRCandidates(cidrRows), pools, snap, nil)
		writes = append(writes, w...)
		findings = append(findings, f...)
		w, f = planRangeCandidates(rangeRows, pools, snap)
		writes = append(writes, w...)
		findings = append(findings, f...)
	}

	return finishReport(writes, findings)
}

func finishReport(writes []WriteEntry, findings []Finding) Report {
	sort.SliceStable(writes, func(i, j int) bool {
		return writeKey(writes[i]) < writeKey(writes[j])
	})
	sort.SliceStable(findings, func(i, j int) bool {
		ac, ar, ax, am := findings[i].sortKey()
		bc, br, bx, bm := findings[j].sortKey()
		if ac != bc {
			return ac < bc
		}
		if ar != br {
			return ar < br
		}
		if ax != bx {
			return ax < bx
		}
		return am < bm
	})
	return Report{Writes: writes, Findings: findings}
}

func writeKey(w WriteEntry) string {
	if w.Kind == WriteRange {
		return "1|" + w.StartAddress + "|" + w.EndAddress
	}
	return "0|" + w.CIDR
}

func findDomain(cfg domain.Config, domainID string) (domain.Domain, bool) {
	for _, d := range cfg.Domains {
		if d.ID == domainID {
			return d, true
		}
	}
	return domain.Domain{}, false
}

// --- accounts ---

func planAccounts(rows []AccountRow) []Finding {
	var findings []Finding
	for _, r := range rows {
		if !accountIDPattern.MatchString(r.AccountID) {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleInvalidAccountID, Rows: []int{r.SourceRow},
				Message: fmt.Sprintf("account id %q is not 12 digits", r.AccountID)})
		}
		if r.RoleARN != "" {
			want := "arn:aws:iam::" + r.AccountID + ":role/"
			if len(r.RoleARN) <= len(want) || r.RoleARN[:len(want)] != want {
				findings = append(findings, Finding{Level: LevelError, Rule: RuleInvalidRoleARN, Rows: []int{r.SourceRow},
					Message: fmt.Sprintf("role_arn %q is not arn:aws:iam::%s:role/<name>", r.RoleARN, r.AccountID)})
			}
		}
	}
	return findings
}

// --- prefix-shaped candidates (networks rows, and ranges rows given as a CIDR) ---

// candidate is one row's contribution of a single CIDR, before duplicates
// across rows are collapsed.
type candidate struct {
	sourceRow int
	raw       string
}

func networkCandidates(rows []NetworkRow) []candidate {
	out := make([]candidate, len(rows))
	for i, r := range rows {
		out[i] = candidate{sourceRow: r.SourceRow, raw: r.CIDR}
	}
	return out
}

func rangeCIDRCandidates(rows []RangeRow) []candidate {
	out := make([]candidate, len(rows))
	for i, r := range rows {
		out[i] = candidate{sourceRow: r.SourceRow, raw: r.CIDR}
	}
	return out
}

// prefixGroup is every source row that resolved to the same canonical CIDR.
type prefixGroup struct {
	cidr netip.Prefix
	rows []int
}

// networkRows is table.Networks when candidates were built from a networks
// table (networkCandidates), and nil when they were built from a ranges
// table's CIDR rows (rangeCIDRCandidates) -- only a networks table's rows can
// ever carry ADR 0016 contributors, so networkRows == nil disables both the
// RuleDuplicateCIDR contributor-identity annotation and the four/five
// contributor findings below, leaving this function's behaviour for ranges
// byte-for-byte what it was before package M9b2.
func planPrefixCandidates(candidates []candidate, pools []domain.Pool, snap domain.InventorySnapshot, networkRows []NetworkRow) ([]WriteEntry, []Finding) {
	var findings []Finding
	groups := map[string]*prefixGroup{}
	var order []string
	for _, c := range candidates {
		p, err := netip.ParsePrefix(c.raw)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleInvalidCIDR, Rows: []int{c.sourceRow},
				CIDR: c.raw, Message: fmt.Sprintf("%q is not a canonical IPv4 CIDR", c.raw)})
			continue
		}
		key := p.String()
		g, ok := groups[key]
		if !ok {
			g = &prefixGroup{cidr: p}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, c.sourceRow)
	}

	var writes []WriteEntry
	for _, key := range order {
		g := groups[key]
		rows := sortedRows(g.rows)
		if len(rows) > 1 {
			msg := fmt.Sprintf("%d rows resolve to the same CIDR %s; collapsed to one write", len(rows), g.cidr)
			// ADR 0016 (M9b2): name the contributors the collapse produces,
			// not just how many rows collapsed -- the gap this whole record
			// exists for is that a shared prefix used to forget everyone but
			// one of them.
			if networkRows != nil {
				if ids := contributorIdentities(rowsForPrefix(networkRows, g.cidr)); len(ids) > 0 {
					msg = fmt.Sprintf("%s, naming contributors %s", msg, strings.Join(ids, ", "))
				}
			}
			findings = append(findings, Finding{Level: LevelWarning, Rule: RuleDuplicateCIDR, Rows: rows, CIDR: g.cidr.String(),
				Message: msg})
		}

		if pool, ok := exactPool(g.cidr, pools); ok {
			findings = append(findings, Finding{Level: LevelError, Rule: RulePoolExactMatch, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s is configured pool %s's own CIDR", g.cidr, pool.ID)})
			continue
		}
		if pool, ok := ancestorOfPool(g.cidr, pools); ok {
			findings = append(findings, Finding{Level: LevelError, Rule: RulePoolAncestor, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s contains pool %s (%s); Snapshot ignores it as an ancestor, so it would block nothing", g.cidr, pool.ID, pool.CIDR)})
			continue
		}
		if net, ok := overlapsManaged(g.cidr, snap); ok {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleOverlapsManaged, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s overlaps platform-managed network %s (allocation %s)", g.cidr, net.CIDR, net.AllocationID)})
			continue
		}
		if parent, ok := insideManagedVPC(g.cidr, snap); ok {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleInsideManagedVPC, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s is inside platform-managed VPC %s (allocation %s); importing it as unmanaged occupancy", g.cidr, parent.CIDR, parent.AllocationID)})
		}
		if net, ok := alreadyUnmanaged(g.cidr, snap); ok {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleAlreadyUnmanaged, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s is already present as unmanaged occupancy; apply would write nothing", g.cidr)})
			// ADR 0016 (M9b2): this is the only place Plan ever has both an
			// existing prefix and a table naming rows for it, so it is the
			// only place a contributor comparison is meaningful -- a CIDR
			// the snapshot does not carry yet has nothing to be "new",
			// "absent" or "unknown" relative to.
			if networkRows != nil {
				findings = append(findings, contributorFindings(rowsForPrefix(networkRows, g.cidr), net)...)
			}
			continue
		}

		writes = append(writes, WriteEntry{Kind: WritePrefix, CIDR: g.cidr.String(), SourceRows: rows})
		if !insideAnyPool(g.cidr, pools) {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleOutsidePool, Rows: rows, CIDR: g.cidr.String(),
				Message: fmt.Sprintf("%s is outside every configured pool", g.cidr)})
		}
	}
	return writes, findings
}

func exactPool(cidr netip.Prefix, pools []domain.Pool) (domain.Pool, bool) {
	for _, p := range pools {
		pc, err := netip.ParsePrefix(p.CIDR)
		if err != nil {
			continue
		}
		if pc.Masked() == cidr {
			return p, true
		}
	}
	return domain.Pool{}, false
}

// ancestorOfPool mirrors internal/netbox/client.go's Snapshot ancestor
// check: `if pc.Bits() > cidr.Bits() && cidr.Contains(pc.Addr())`, read here
// with cidr as the candidate row and pc as the pool.
func ancestorOfPool(cidr netip.Prefix, pools []domain.Pool) (domain.Pool, bool) {
	for _, p := range pools {
		pc, err := netip.ParsePrefix(p.CIDR)
		if err != nil {
			continue
		}
		pc = pc.Masked()
		if pc.Bits() > cidr.Bits() && cidr.Contains(pc.Addr()) {
			return p, true
		}
	}
	return domain.Pool{}, false
}

func insideAnyPool(cidr netip.Prefix, pools []domain.Pool) bool {
	for _, p := range pools {
		pc, err := netip.ParsePrefix(p.CIDR)
		if err != nil {
			continue
		}
		pc = pc.Masked()
		if pc.Bits() <= cidr.Bits() && pc.Contains(cidr.Addr()) {
			return true
		}
	}
	return false
}

func overlapsManaged(cidr netip.Prefix, snap domain.InventorySnapshot) (domain.Network, bool) {
	for _, n := range snap.Networks {
		if n.AllocationID == "" {
			continue
		}
		nc, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if !cidr.Overlaps(nc) {
			continue
		}
		if childOfManagedVPC(cidr, nc, n) {
			continue
		}
		return n, true
	}
	return domain.Network{}, false
}

// childOfManagedVPC reports whether a candidate lies strictly inside the
// prefix of a managed **vpc-scoped** allocation -- the one overlap with
// managed space an import may write (RuleInsideManagedVPC).
//
// "Strictly inside" is the whole guard: equality would be a second prefix at a
// CIDR the snapshot already holds, which takes the domain's whole snapshot
// down (RuleDuplicateCIDR's comment), and a candidate that merely contains or
// straddles the managed prefix describes space the platform has already issued
// to somebody. A managed **subnet** is a leaf as far as this project models
// address space, so nothing may be imported inside one either.
func childOfManagedVPC(cidr, managed netip.Prefix, n domain.Network) bool {
	if n.ParentAllocationID != "" || n.ParentPool {
		return false
	}
	managed = managed.Masked()
	return cidr.Bits() > managed.Bits() && managed.Contains(cidr.Addr())
}

// insideManagedVPC finds the managed vpc-scoped network a candidate is a child
// of, for the informational finding that names it. It is asked only after
// overlapsManaged has already said the candidate is admissible, so at most one
// managed network can contain it: two managed prefixes containing the same
// candidate would mean one contains the other, and a managed network strictly
// inside another managed network is a subnet, which childOfManagedVPC refuses.
func insideManagedVPC(cidr netip.Prefix, snap domain.InventorySnapshot) (domain.Network, bool) {
	for _, n := range snap.Networks {
		if n.AllocationID == "" {
			continue
		}
		nc, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if cidr.Overlaps(nc) && childOfManagedVPC(cidr, nc, n) {
			return n, true
		}
	}
	return domain.Network{}, false
}

// alreadyUnmanaged returns the snapshot's unmanaged network at cidr, if any.
// It returns the matched domain.Network (not just a bool) so a caller can
// read its Contributors and ContributorsUnreadable -- package M9b2 needs
// exactly that to compare a re-import's rows against what the prefix already
// carries (ADR 0016).
func alreadyUnmanaged(cidr netip.Prefix, snap domain.InventorySnapshot) (domain.Network, bool) {
	for _, n := range snap.Networks {
		if n.AllocationID != "" || n.ParentPool {
			continue
		}
		nc, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if nc == cidr {
			return n, true
		}
	}
	return domain.Network{}, false
}

// --- ADR 0016 (package M9b2): contributor findings ---

// rowsForPrefix returns every NetworkRow whose CIDR normalizes to cidr, in
// their original order -- the same grouping planPrefixCandidates itself
// performs to build one WriteEntry.SourceRows per canonical CIDR, exposed
// here so the duplicate-CIDR message and the contributor comparison can
// rebuild ADR 0016's contributor list for the group. A row whose CIDR does
// not parse as a canonical IPv4 prefix is silently excluded: planPrefixCandidates
// has already reported it as its own RuleInvalidCIDR finding, via
// networkCandidates/the candidate loop above, so it never joined any group
// and re-reporting it here would be a second, redundant finding.
func rowsForPrefix(rows []NetworkRow, cidr netip.Prefix) []NetworkRow {
	var out []NetworkRow
	for _, r := range rows {
		p, err := netip.ParsePrefix(r.CIDR)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			continue
		}
		if p == cidr {
			out = append(out, r)
		}
	}
	return out
}

// contributorIdentities is the distinct, ordered identity strings
// ContributorsForRows would build for rows -- used only to annotate
// RuleDuplicateCIDR's message (ADR 0016: "RuleDuplicateCIDR's message gains
// the contributor identities").
func contributorIdentities(rows []NetworkRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range ContributorsForRows(rows, "") {
		if seen[c.Identity] {
			continue
		}
		seen[c.Identity] = true
		out = append(out, c.Identity)
	}
	return out
}

// contributorFindings compares ADR 0016's contributor lists for one prefix
// the snapshot already carries as unmanaged occupancy against what this
// table's own rows for the same CIDR would write (built by
// ContributorsForRows -- the same function internal/onboardcmd's
// entryOccupancy calls on create, so the two can never disagree about what a
// table "would write"). It is called only from the branch above where
// alreadyUnmanaged has already found net at this exact CIDR: with no
// existing prefix there is nothing to compare against, and a table's own
// rows are then simply this import's first write, not a "new" or "absent"
// contributor relative to anything.
//
// A degenerate identity -- ADR 0016's and the M9b1 review's word for the
// identity string a row with no resource_id produces
// ("aws:<account>:<region>::<cidr>", the VPC segment empty) -- can match
// only itself and is never treated as evidence in either direction:
//   - it never raises RuleContributorNew, because a different VPC could be
//     hiding behind the same collapsed identity, so "the prefix does not
//     carry it yet" cannot be shown;
//   - an existing entry with a degenerate identity never raises
//     RuleContributorAbsent, because the table's own rows could be the same
//     resource under the same collapsed identity, so "gone" cannot be shown
//     either.
//
// This is docs/WORK_PLAN.md's M9b1 review note, applied here rather than
// left for a reader to rediscover: "a table without a resource_id column ...
// yields contributors sharing one degenerate identity, which M9b2 must treat
// like a null observation time" -- unusable as evidence, not absent from the
// report (RuleContributorStaleSource is still raised for a degenerate row
// with a null observed_at, exactly as for any other row: that finding is
// about the row's own provenance, not a comparison outcome).
func contributorFindings(rows []NetworkRow, net domain.Network) []Finding {
	cidr := net.CIDR

	// ADR 0016's package M9b2 block: "An unreadable list is its own case
	// (not contributor-unknown, never reconstructed silently)." Nothing else
	// is computed for this CIDR: an unreadable field cannot be compared
	// against, so there is no basis for "new" or "absent" either.
	if net.ContributorsUnreadable {
		return []Finding{{
			Level: LevelWarning, Rule: RuleContributorUnreadable, CIDR: cidr,
			Message: fmt.Sprintf("%s: platform_import_contributors could not be decoded as a contributor list; treated as unknown, never reconstructed silently", cidr),
		}}
	}
	// A nil Contributors (as opposed to a non-nil, possibly empty slice) is
	// ADR 0016's "imported before this record": there is no list to compare
	// against at all, so contributor-new/absent/stale-source are all
	// meaningless here -- only a refresh (M9b3) can decide what belongs.
	if net.Contributors == nil {
		return []Finding{{
			Level: LevelInfo, Rule: RuleContributorUnknown, CIDR: cidr,
			Message: fmt.Sprintf("%s carries no contributor list; it was imported before ADR 0016, and a refresh would reconstruct one from this table", cidr),
		}}
	}

	have := map[string]bool{}
	for _, c := range net.Contributors {
		have[c.Identity] = true
	}

	want := ContributorsForRows(rows, "")
	wantIdentity := map[string]bool{}
	for _, c := range want {
		wantIdentity[c.Identity] = true
	}

	var findings []Finding
	seenNew := map[string]bool{}
	for _, c := range want {
		degenerate := c.ResourceID == ""
		if !degenerate && !have[c.Identity] && !seenNew[c.Identity] {
			seenNew[c.Identity] = true
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleContributorNew, Rows: []int{c.SourceRow}, CIDR: cidr,
				Message: fmt.Sprintf("%s: %s is a contributor this table names that the prefix does not carry yet", cidr, c.Identity)})
		}
		if c.ObservedAt == nil {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleContributorStaleSource, Rows: []int{c.SourceRow}, CIDR: cidr,
				Message: fmt.Sprintf("%s: %s has no recorded observation time and could never be removal evidence", cidr, c.Identity)})
		}
	}

	seenAbsent := map[string]bool{}
	for _, c := range net.Contributors {
		degenerate := c.ResourceID == ""
		if degenerate || wantIdentity[c.Identity] || seenAbsent[c.Identity] {
			continue
		}
		seenAbsent[c.Identity] = true
		findings = append(findings, Finding{Level: LevelWarning, Rule: RuleContributorAbsent, CIDR: cidr,
			Message: fmt.Sprintf("%s: %s is a recorded contributor this table does not name; a table is a scope, not a census, so nothing is removed", cidr, c.Identity)})
	}
	return findings
}

// --- start/end range candidates ---

type rangeGroup struct {
	start, end netip.Addr
	rows       []int
}

func planRangeCandidates(rows []RangeRow, pools []domain.Pool, snap domain.InventorySnapshot) ([]WriteEntry, []Finding) {
	var findings []Finding
	groups := map[string]*rangeGroup{}
	var order []string
	for _, r := range rows {
		start, errS := netip.ParseAddr(r.StartAddress)
		end, errE := netip.ParseAddr(r.EndAddress)
		if errS != nil || errE != nil || !start.Is4() || !end.Is4() {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleInvalidCIDR, Rows: []int{r.SourceRow},
				Message: fmt.Sprintf("range %s-%s is not a valid IPv4 address pair", r.StartAddress, r.EndAddress)})
			continue
		}
		if start.Compare(end) > 0 {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleInvalidCIDR, Rows: []int{r.SourceRow},
				Message: fmt.Sprintf("range %s-%s ends before it starts", start, end)})
			continue
		}
		key := start.String() + "-" + end.String()
		g, ok := groups[key]
		if !ok {
			g = &rangeGroup{start: start, end: end}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, r.SourceRow)
	}

	var writes []WriteEntry
	for _, key := range order {
		g := groups[key]
		rows := sortedRows(g.rows)
		cidrLabel := g.start.String() + "-" + g.end.String()
		if len(rows) > 1 {
			findings = append(findings, Finding{Level: LevelWarning, Rule: RuleDuplicateCIDR, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("%d rows resolve to the same range %s; collapsed to one write", len(rows), cidrLabel)})
		}

		if pool, ok := spansPool(g.start, g.end, pools); ok {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleRangeSpansPool, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("range %s spans pool %s (%s)", cidrLabel, pool.ID, pool.CIDR)})
			continue
		}
		if net, ok := rangeOverlapsManaged(g.start, g.end, snap); ok {
			findings = append(findings, Finding{Level: LevelError, Rule: RuleOverlapsManaged, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("range %s overlaps platform-managed network %s (allocation %s)", cidrLabel, net.CIDR, net.AllocationID)})
			continue
		}
		if parent, ok := rangeInsideManagedVPC(g.start, g.end, snap); ok {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleInsideManagedVPC, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("range %s is inside platform-managed VPC %s (allocation %s); importing it as unmanaged occupancy", cidrLabel, parent.CIDR, parent.AllocationID)})
		}

		writes = append(writes, WriteEntry{Kind: WriteRange, StartAddress: g.start.String(), EndAddress: g.end.String(), SourceRows: rows})

		if blocks, hitCap := rangeBlockCount(g.start, g.end, maxOnboardRangeBlocks); hitCap {
			findings = append(findings, Finding{Level: LevelWarning, Rule: RuleRangeTooLarge, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("range %s needs more than %d CIDR blocks (found %d); a later snapshot substitutes one covering, over-blocking prefix", cidrLabel, maxOnboardRangeBlocks, blocks)})
		}
		if !rangeInsideAnyPool(g.start, g.end, pools) {
			findings = append(findings, Finding{Level: LevelInfo, Rule: RuleOutsidePool, Rows: rows, CIDR: cidrLabel,
				Message: fmt.Sprintf("range %s is outside every configured pool", cidrLabel)})
		}
	}
	return writes, findings
}

// spansPool mirrors internal/netbox/occupancy.go's refusePoolSpan: a range
// that starts at or before a pool's first address and ends at or after its
// last would, once decomposed by Snapshot's rangeCIDRs, mark the pool's
// entire address space occupied.
func spansPool(start, end netip.Addr, pools []domain.Pool) (domain.Pool, bool) {
	for _, p := range pools {
		pc, err := netip.ParsePrefix(p.CIDR)
		if err != nil {
			continue
		}
		pc = pc.Masked()
		if start.Compare(pc.Addr()) <= 0 && end.Compare(lastAddr(pc)) >= 0 {
			return p, true
		}
	}
	return domain.Pool{}, false
}

func rangeInsideAnyPool(start, end netip.Addr, pools []domain.Pool) bool {
	for _, p := range pools {
		pc, err := netip.ParsePrefix(p.CIDR)
		if err != nil {
			continue
		}
		pc = pc.Masked()
		if pc.Contains(start) && pc.Contains(end) {
			return true
		}
	}
	return false
}

func rangeOverlapsManaged(start, end netip.Addr, snap domain.InventorySnapshot) (domain.Network, bool) {
	for _, n := range snap.Networks {
		if n.AllocationID == "" {
			continue
		}
		nc, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if start.Compare(lastAddr(nc)) > 0 || end.Compare(nc.Addr()) < 0 {
			continue
		}
		if rangeChildOfManagedVPC(start, end, nc, n) {
			continue
		}
		return n, true
	}
	return domain.Network{}, false
}

// rangeChildOfManagedVPC is childOfManagedVPC for a start/end range: the same
// rule, with "strictly inside" meaning the range fits within the managed VPC's
// prefix without covering the whole of it.
func rangeChildOfManagedVPC(start, end netip.Addr, managed netip.Prefix, n domain.Network) bool {
	if n.ParentAllocationID != "" || n.ParentPool {
		return false
	}
	managed = managed.Masked()
	first, last := managed.Addr(), lastAddr(managed)
	if start.Compare(first) < 0 || end.Compare(last) > 0 {
		return false
	}
	return start.Compare(first) > 0 || end.Compare(last) < 0
}

func rangeInsideManagedVPC(start, end netip.Addr, snap domain.InventorySnapshot) (domain.Network, bool) {
	for _, n := range snap.Networks {
		if n.AllocationID == "" {
			continue
		}
		nc, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			continue
		}
		if rangeChildOfManagedVPC(start, end, nc, n) {
			return n, true
		}
	}
	return domain.Network{}, false
}

// lastAddr returns a prefix's last (broadcast) address. Mirrors
// internal/netbox/client.go's prefixLast, restricted to IPv4 since this
// package only ever handles IPv4 (design section 4: IPv6 is a warning and
// the row is skipped before Plan is reached).
func lastAddr(p netip.Prefix) netip.Addr {
	v := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	for i := 0; i < host; i++ {
		v[3-i/8] |= 1 << uint(i%8)
	}
	return netip.AddrFrom4(v)
}

// rangeBlockCount decomposes [start, end] into CIDR blocks using the same
// greedy "largest aligned block that still fits" algorithm as
// internal/netbox/client.go's rangeCIDRs, stopping at cap the same way. It
// exists to let Plan warn before apply, without importing internal/netbox or
// duplicating the full decomposition (Plan needs only whether the cap would
// be hit, not the resulting blocks).
func rangeBlockCount(start, end netip.Addr, limit int) (blocks int, hitCap bool) {
	cur := start
	for cur.Compare(end) <= 0 && blocks < limit {
		var chosen netip.Prefix
		for bits := 0; bits <= cur.BitLen(); bits++ {
			p := netip.PrefixFrom(cur, bits).Masked()
			if p.Addr().Compare(cur) == 0 && lastAddr(p).Compare(end) <= 0 {
				chosen = p
				break
			}
		}
		if !chosen.IsValid() {
			// Unreachable for a validated start<=end pair: bits==cur.BitLen()
			// always yields a single-host block that trivially satisfies both
			// conditions above.
			blocks++
			break
		}
		blocks++
		next := lastAddr(chosen).Next()
		if !next.IsValid() {
			break
		}
		cur = next
	}
	return blocks, cur.Compare(end) <= 0
}

func sortedRows(rows []int) []int {
	out := append([]int(nil), rows...)
	sort.Ints(out)
	return out
}
