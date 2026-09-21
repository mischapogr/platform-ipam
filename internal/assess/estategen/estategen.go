// Package estategen is the synthetic estate generator work-plan package
// M1b4 (docs/WORK_PLAN.md, docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md)
// builds: it writes a whole collector-shaped organization inventory
// directory -- networks.csv (the collector's thirteen columns,
// scripts/aws/org-inventory.sh), accounts.json, failures.csv and run.json --
// plus matching reviewed inputs (a connectivity matrix, an ownership table,
// a fixed-range table and a decisions file, all as YAML) deterministically
// from a seed and a small set of size parameters, with a set of KNOWN
// planted facts returned alongside the files.
//
// It is a test and demonstration tool, not part of the product. It is
// deliberately NOT wired into cmd/platform-ipam's command surface -- nothing
// under cmd/ imports this package -- and it adds no dependency: everything
// it writes is built with encoding/csv, encoding/json and fmt from the
// standard library, plus this repository's own internal/assess (for
// ResourceRecord.Identity and ConflictID, so the facts this package predicts
// are computed the SAME way internal/assess computes them, not by a second,
// possibly drifting formula).
//
// CONFIDENTIALITY: every account id, VPC id, association id and CIDR this
// package writes is synthetic. Account ids are computed arithmetically from
// the literal base 100_000_000_000 (see acctID) -- the same convention
// internal/assess's own genEstate test helper uses (internal/assess/estate_test.go)
// -- never a copy-pasted real account id. Every CIDR is RFC 1918 (10.0.0.0/8,
// 172.16.0.0/12) or RFC 5737 documentation space (192.0.2.0/24). Every VPC,
// subnet and association id carries an obviously fake "vpc-", "subnet-" or
// "vpc-cidr-assoc-" prefix over a zero-padded decimal, never AWS's own hex
// id shape. TestGeneratedFilesContainNoTwelveDigitSequenceOtherThanSynthetic
// in estategen_test.go is this package's own version of
// internal/assess/assess_test.go's TestFixturesUseOnlySyntheticAccountIDs:
// it scans every file this package WROTE (not just Go source) for a
// twelve-digit sequence and asserts each one is one of the account ids
// Generate itself returns in Facts.Accounts, closing the gap that Go's own
// literal-scanning guard cannot reach (nothing here is a Go string literal;
// every id is written to a file at runtime).
//
// This package does not read anything under inventory/ (the .gitignored
// directory a real organization inventory lands in, per
// docs/AWS_ORGANIZATION_INVENTORY.md's own warning) and never will: every
// byte it writes is generated, not copied.
package estategen

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// regionPool is the fixed, small set of region names Generate draws from.
// Real AWS region names, but nothing here calls AWS; they are chosen only
// so a worked example in documentation reads naturally.
var regionPool = []string{"eu-central-1", "us-east-1", "ap-southeast-1"}

// Params sizes and seeds one call to Generate. All fields have the same
// meaning for both the "gapped" estate (Gapped true: every coverage gap and
// every conflict kind this package plants) and the "clean" one (Gapped
// false, ordinarily paired with EqualCIDR = Contains = 0): the same code
// path produces both, so a difference between them is a difference in
// Params, never in two separate generators that could drift apart.
type Params struct {
	// Seed is accepted for forward compatibility with a future version
	// that varies the filler layout across calls; the current
	// implementation's layout is already a deterministic function of the
	// other fields below and does not yet need to consult it.
	Seed int64

	// BaselineAccounts is the number of ordinary, fully-read accounts:
	// every region of every one of them gets VPCsPerRegion filler VPCs
	// with unique, non-conflicting CIDRs, and (when Gapped) hosts the
	// planted conflicts, the secondary association, the duplicate
	// observation and the subnets. Must be at least 4 when Gapped is true
	// and EqualCIDR or Contains is nonzero, so every planted pair and its
	// coverage-gap regions have distinct baseline accounts to live in;
	// Generate returns an error rather than silently reusing one.
	BaselineAccounts  int
	RegionsPerAccount int
	VPCsPerRegion     int
	// SubnetsUnderOneVPC: how many subnet rows Generate attaches to one
	// designated baseline VPC (Facts.SubnetParentVPC). Zero omits the
	// "VPC with subnets" planted fact entirely.
	SubnetsUnderOneVPC int

	// EqualCIDR (N) and Contains (M) are the number of planted equal-cidr
	// and contains relationships, each between two distinct, named VPCs
	// in two distinct baseline accounts. Zero is a valid estate with no
	// conflicts at all (the shape the "clean" estate uses).
	EqualCIDR int
	Contains  int

	// Gapped plants the seven facts ADR 0014's evidence paragraph and
	// docs/WORK_PLAN.md's M1b4 block name together: a secondary
	// association, a duplicate observation, a VPC with subnets
	// (SubnetsUnderOneVPC must be nonzero for this one to appear), one
	// account that could not be assumed, one region that is partial and
	// populated, one region read and empty, and one ACTIVE account never
	// attempted at all. Coverage.Complete is unconditionally false for
	// such an estate. When Gapped is false none of the seven is planted,
	// run.json and accounts.json cover the whole baseline estate exactly,
	// and Coverage.Complete is unconditionally true.
	Gapped bool
}

// EqualCIDRFact names one planted equal-cidr relationship: two VPCs, in two
// different accounts and (by construction) the same region, sharing one
// CIDR.
type EqualCIDRFact struct {
	AccountA, AccountB string
	VPCA, VPCB         string
	Region             string
	CIDR               string
	// ConflictID is the id internal/assess.ConflictID computes for this
	// pair, from the same two resource identities Generate wrote to
	// networks.csv.
	ConflictID string
}

// ContainsFact names one planted contains relationship: an outer VPC whose
// CIDR strictly contains an inner VPC's, in two different accounts and the
// same region.
type ContainsFact struct {
	AccountOuter, AccountInner string
	VPCOuter, VPCInner         string
	Region                     string
	OuterCIDR, InnerCIDR       string
	ConflictID                 string
}

// AccountRegion names one account/region pair, e.g. for a coverage gap.
type AccountRegion struct {
	AccountID string
	Region    string
}

// AssociationFact names one VPC CIDR association Generate added outside the
// baseline filler loop (the secondary association and the duplicated
// observation both use this shape).
type AssociationFact struct {
	AccountID, Region, VPCID, CIDR, AssociationID string
}

// Facts is everything Generate plants and predicts about its own output:
// docs/WORK_PLAN.md's M1b4 block requires "the generator's own tests assert
// the planted facts come back out of `onboard assess`", and this is the
// value a test compares a produced Report against.
type Facts struct {
	// Accounts lists every ACTIVE account id Generate wrote to
	// accounts.json, in the same order, including the unassumable and
	// never-attempted accounts when Gapped is true.
	Accounts []string

	EqualCIDR []EqualCIDRFact
	Contains  []ContainsFact

	// Secondary is set only when Gapped is true: a second, secondary
	// (Primary=false) association added to EqualCIDR[0]'s A side, with
	// its own association id -- a VPC legitimately holding two associated
	// CIDRs, occupying space on both.
	Secondary *AssociationFact

	// Duplicate is set only when Gapped is true: one baseline filler
	// association written TWICE, byte-identical account/region/VPC/CIDR/
	// association id, which internal/assess collapses to one observation
	// and counts.
	Duplicate *AssociationFact

	// SubnetParentVPC and SubnetIDs are set only when Gapped is true and
	// Params.SubnetsUnderOneVPC is nonzero.
	SubnetParentVPC string
	SubnetIDs       []string

	// UnassumableAccount is set only when Gapped is true: one account id
	// present in accounts.json (Status ACTIVE) that fails assume-role
	// entirely -- a failures.csv row, and a run.json entry with
	// regions_attempted "unknown" and not_attempted_reason
	// "assume-role-failed" -- and contributes no row to networks.csv.
	UnassumableAccount string

	// PartialRegion is set only when Gapped is true: one account/region
	// pair whose VPC rows ARE present in networks.csv (the collector's own
	// "VPC call succeeded, subnet call failed" behaviour) alongside a
	// failures.csv "describe" row and a run.json "partial" outcome for
	// that same pair.
	PartialRegion *AccountRegion

	// EmptyRegion is set only when Gapped is true: one account/region pair
	// with ZERO rows in networks.csv and a run.json outcome of
	// "succeeded" with row_count 0 -- "read and empty", not "not read".
	EmptyRegion *AccountRegion

	// NeverAttemptedAccount is set only when Gapped is true: one ACTIVE
	// account present in accounts.json that is entirely absent from
	// run.json's own accounts list (unlike UnassumableAccount, which
	// run.json DOES mention, as not_attempted) and has no failures.csv
	// row either -- the case ADR 0014's evidence paragraph names
	// separately: "an ACTIVE account in accounts.json that was never
	// attempted".
	NeverAttemptedAccount string

	// ConfirmedConflictID, IsolatedConflictID and UnknownConflictID name
	// which of EqualCIDR/Contains' conflict ids the reviewed matrix.yaml
	// this package writes classifies as confirmed, potential (via
	// must_stay_isolated) and unknown, respectively -- present only when
	// the corresponding pair exists.
	ConfirmedConflictID string
	IsolatedConflictID  string
	UnknownConflictID   string

	// TotalVPCToVPCRelationships is len(EqualCIDR) + len(Contains): the
	// exact count a produced Report.Summary.TotalRelationships must equal.
	TotalVPCToVPCRelationships int
}

// networkRow is one row of networks.csv, in column order.
type networkRow struct {
	accountID, accountName, region, typ, resourceID, cidr string
	parentID, azID, name, state, primary                  string
	associationID, observedAt                             string
}

const observedAt = "2026-09-20T00:00:00Z"

// acctID is the only place a twelve-digit sequence is deliberately written:
// every other id below stays under twelve consecutive digits specifically
// so a naive twelve-digit scan (this package's own
// TestGeneratedFilesContainNoTwelveDigitSequenceOtherThanSynthetic, and
// internal/assess/assess_test.go's guard on Go source) never mistakes a
// zero-padded VPC, subnet or association id for an account id.
func acctID(i int) string   { return fmt.Sprintf("%012d", 100000000000+int64(i)) }
func vpcID(n int) string    { return fmt.Sprintf("vpc-%010d", n) }
func subnetID(n int) string { return fmt.Sprintf("subnet-%010d", n) }
func assocID(n int) string  { return fmt.Sprintf("vpc-cidr-assoc-%010d", n) }

// uniquePrefix derives a deterministic, collision-free IPv4 prefix of the
// given length from an integer index, walking base in blocks of that size.
func uniquePrefix(base netip.Addr, index, bits int) netip.Prefix {
	a4 := base.As4()
	start := uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
	blockSize := uint32(1) << uint(32-bits)
	addr := start + uint32(index)*blockSize
	return prefixFromUint32(addr, bits)
}

// subnetOf returns the index-th sub-block of p, extraBits longer than p's
// own prefix length (capped at /30).
func subnetOf(p netip.Prefix, index, extraBits int) netip.Prefix {
	bits := p.Bits() + extraBits
	if bits > 30 {
		bits = 30
	}
	step := uint32(1) << uint(32-bits)
	a4 := p.Addr().As4()
	base := uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
	return prefixFromUint32(base+uint32(index)*step, bits)
}

func prefixFromUint32(addr uint32, bits int) netip.Prefix {
	var b [4]byte
	b[0] = byte(addr >> 24)
	b[1] = byte(addr >> 16)
	b[2] = byte(addr >> 8)
	b[3] = byte(addr)
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
}

// regionRuns tracks, per account and region, how many networks.csv rows
// landed there and what run.json should say the outcome was.
type regionRuns map[string]map[string]*regionRun

type regionRun struct {
	rows    int
	outcome string // succeeded | partial
}

func (rr regionRuns) bump(acct, region string, n int) {
	m := rr[acct]
	if m == nil {
		return
	}
	e := m[region]
	if e == nil {
		e = &regionRun{outcome: "succeeded"}
		m[region] = e
	}
	e.rows += n
}

// Generate writes a full collector-shaped inventory directory and its
// matching reviewed inputs under outDir (created if it does not exist) and
// returns the facts it planted. It performs no network I/O, reads nothing
// outside p, and calls neither time.Now nor any source of randomness: the
// whole layout is a deterministic function of p.
func Generate(p Params, outDir string) (Facts, error) {
	if p.BaselineAccounts < 1 {
		return Facts{}, fmt.Errorf("estategen: BaselineAccounts must be at least 1")
	}
	if p.Gapped && p.BaselineAccounts < 4 {
		return Facts{}, fmt.Errorf("estategen: Gapped needs at least 4 baseline accounts (one each for the empty region, the partial region and two spares for the planted pairs), got %d", p.BaselineAccounts)
	}
	if p.RegionsPerAccount < 1 || p.RegionsPerAccount > len(regionPool) {
		return Facts{}, fmt.Errorf("estategen: RegionsPerAccount must be between 1 and %d", len(regionPool))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Facts{}, err
	}

	var facts Facts
	var rows []networkRow
	var failureRows [][5]string // account_id, account_name, region, stage, error
	seq := 0
	nextAssoc := func() string { seq++; return assocID(seq) }
	nextVPC := func() string { seq++; return vpcID(seq) }
	nextSubnet := func() string { seq++; return subnetID(seq) }

	fillerBase := netip.MustParseAddr("10.0.0.0")

	accountIDs := make([]string, p.BaselineAccounts)
	for i := range accountIDs {
		accountIDs[i] = acctID(i)
	}
	facts.Accounts = append([]string{}, accountIDs...)

	var emptyAcct, partialAcct string
	var emptyRegion, partialRegion string
	if p.Gapped {
		emptyAcct = accountIDs[len(accountIDs)-1]
		partialAcct = accountIDs[len(accountIDs)-2]
		emptyRegion = regionPool[p.RegionsPerAccount-1]
		partialRegion = regionPool[p.RegionsPerAccount-1]
	}

	runs := regionRuns{}
	for _, a := range accountIDs {
		runs[a] = map[string]*regionRun{}
	}

	var fillerForDuplicate *networkRow
	fillerIndex := 0
	for ai, a := range accountIDs {
		for ri := 0; ri < p.RegionsPerAccount; ri++ {
			region := regionPool[ri]
			isEmpty := p.Gapped && a == emptyAcct && region == emptyRegion
			isPartial := p.Gapped && a == partialAcct && region == partialRegion
			if !isEmpty {
				for v := 0; v < p.VPCsPerRegion; v++ {
					pfx := uniquePrefix(fillerBase, fillerIndex, 20)
					fillerIndex++
					vid := nextVPC()
					row := networkRow{
						accountID: a, accountName: fmt.Sprintf("acct-%d", ai), region: region,
						typ: "vpc", resourceID: vid, cidr: pfx.String(),
						name: fmt.Sprintf("%s-vpc-%d", region, v), state: "available", primary: "true",
						associationID: nextAssoc(), observedAt: observedAt,
					}
					rows = append(rows, row)
					runs.bump(a, region, 1)
					if fillerForDuplicate == nil && p.Gapped && !isPartial {
						r := row
						fillerForDuplicate = &r
					}
				}
			} else {
				runs.bump(a, region, 0) // ensure the region entry exists with outcome "succeeded", row_count 0
			}
			if isPartial {
				runs[a][region].outcome = "partial"
			}
		}
	}

	if p.Gapped {
		facts.EmptyRegion = &AccountRegion{AccountID: emptyAcct, Region: emptyRegion}
		facts.PartialRegion = &AccountRegion{AccountID: partialAcct, Region: partialRegion}
		failureRows = append(failureRows, [5]string{partialAcct, "acct-partial", partialRegion, "describe", "failed"})
	}

	// --- planted equal-cidr pairs.
	eqBase := netip.MustParseAddr("10.200.0.0")
	usable := len(accountIDs)
	if p.Gapped {
		usable -= 2 // the empty- and partial-region accounts stay out of the planted pairs
	}
	// pick never wraps: every planted pair (equal-cidr and contains alike)
	// gets two accounts NO OTHER planted pair uses. Wrapping (accountIDs[i
	// % usable]) would let two different pairs share an account, and
	// because the matrix this package writes assigns a connectivity group
	// by ACCOUNT id (ADR 0014's precedence: explicit VPC id, then product,
	// then account id -- this package uses only the account-id level), an
	// account claimed by two groups is ambiguous and every conflict
	// touching it collapses to impact unknown, silently defeating the
	// three-valued-impact fact this generator exists to plant.
	needed := 2 * (p.EqualCIDR + p.Contains)
	if usable < needed {
		return Facts{}, fmt.Errorf("estategen: %d ordinary baseline accounts are not enough for %d planted pairs, which need %d distinct accounts (2 each, never shared)", usable, p.EqualCIDR+p.Contains, needed)
	}
	pick := func(i int) string { return accountIDs[i] }

	for i := 0; i < p.EqualCIDR; i++ {
		accA, accB := pick(2*i), pick(2*i+1)
		region := regionPool[0]
		cidr := uniquePrefix(eqBase, i, 24).String()
		vidA, vidB := nextVPC(), nextVPC()
		assocA, assocB := nextAssoc(), nextAssoc()
		rows = append(rows,
			networkRow{accountID: accA, accountName: "acct-" + accA, region: region, typ: "vpc", resourceID: vidA, cidr: cidr,
				name: fmt.Sprintf("equal-cidr-%d-a", i), state: "available", primary: "true", associationID: assocA, observedAt: observedAt},
			networkRow{accountID: accB, accountName: "acct-" + accB, region: region, typ: "vpc", resourceID: vidB, cidr: cidr,
				name: fmt.Sprintf("equal-cidr-%d-b", i), state: "available", primary: "true", associationID: assocB, observedAt: observedAt},
		)
		runs.bump(accA, region, 1)
		runs.bump(accB, region, 1)
		idA := assess.ResourceRecord{AccountID: accA, Region: region, ResourceID: vidA, CIDR: cidr, AssociationID: &assocA}.Identity()
		idB := assess.ResourceRecord{AccountID: accB, Region: region, ResourceID: vidB, CIDR: cidr, AssociationID: &assocB}.Identity()
		fact := EqualCIDRFact{AccountA: accA, AccountB: accB, VPCA: vidA, VPCB: vidB, Region: region, CIDR: cidr,
			ConflictID: assess.ConflictID(assess.KindEqualCIDR, idA, idB)}
		facts.EqualCIDR = append(facts.EqualCIDR, fact)

		if i == 0 && p.Gapped {
			secCIDR := uniquePrefix(netip.MustParseAddr("10.220.0.0"), 0, 20).String()
			secAssoc := nextAssoc()
			rows = append(rows, networkRow{accountID: accA, accountName: "acct-" + accA, region: region, typ: "vpc",
				resourceID: vidA, cidr: secCIDR, name: "equal-cidr-0-a-secondary", state: "available", primary: "false",
				associationID: secAssoc, observedAt: observedAt})
			runs.bump(accA, region, 1)
			facts.Secondary = &AssociationFact{AccountID: accA, Region: region, VPCID: vidA, CIDR: secCIDR, AssociationID: secAssoc}

			if p.SubnetsUnderOneVPC > 0 {
				facts.SubnetParentVPC = vidA
				for s := 0; s < p.SubnetsUnderOneVPC; s++ {
					sp := subnetOf(netip.MustParsePrefix(cidr), s, 4)
					sid := nextSubnet()
					rows = append(rows, networkRow{accountID: accA, accountName: "acct-" + accA, region: region, typ: "subnet",
						resourceID: sid, cidr: sp.String(), parentID: vidA, azID: region + "a",
						name: fmt.Sprintf("subnet-%d", s), state: "available", observedAt: observedAt})
					runs.bump(accA, region, 1)
					facts.SubnetIDs = append(facts.SubnetIDs, sid)
				}
			}
		}
	}

	// --- planted contains pairs.
	for i := 0; i < p.Contains; i++ {
		accOuter, accInner := pick(2*(p.EqualCIDR+i)), pick(2*(p.EqualCIDR+i)+1)
		region := regionPool[0]
		outer := fmt.Sprintf("10.%d.0.0/16", 212+i)
		inner := fmt.Sprintf("10.%d.5.0/24", 212+i)
		vidOuter, vidInner := nextVPC(), nextVPC()
		assocOuter, assocInner := nextAssoc(), nextAssoc()
		rows = append(rows,
			networkRow{accountID: accOuter, accountName: "acct-" + accOuter, region: region, typ: "vpc", resourceID: vidOuter, cidr: outer,
				name: fmt.Sprintf("contains-%d-outer", i), state: "available", primary: "true", associationID: assocOuter, observedAt: observedAt},
			networkRow{accountID: accInner, accountName: "acct-" + accInner, region: region, typ: "vpc", resourceID: vidInner, cidr: inner,
				name: fmt.Sprintf("contains-%d-inner", i), state: "available", primary: "true", associationID: assocInner, observedAt: observedAt},
		)
		runs.bump(accOuter, region, 1)
		runs.bump(accInner, region, 1)
		idOuter := assess.ResourceRecord{AccountID: accOuter, Region: region, ResourceID: vidOuter, CIDR: outer, AssociationID: &assocOuter}.Identity()
		idInner := assess.ResourceRecord{AccountID: accInner, Region: region, ResourceID: vidInner, CIDR: inner, AssociationID: &assocInner}.Identity()
		facts.Contains = append(facts.Contains, ContainsFact{
			AccountOuter: accOuter, AccountInner: accInner, VPCOuter: vidOuter, VPCInner: vidInner,
			Region: region, OuterCIDR: outer, InnerCIDR: inner,
			ConflictID: assess.ConflictID(assess.KindContains, idOuter, idInner),
		})
	}
	facts.TotalVPCToVPCRelationships = len(facts.EqualCIDR) + len(facts.Contains)

	if p.Gapped && fillerForDuplicate != nil {
		rows = append(rows, *fillerForDuplicate)
		runs.bump(fillerForDuplicate.accountID, fillerForDuplicate.region, 1)
		facts.Duplicate = &AssociationFact{
			AccountID: fillerForDuplicate.accountID, Region: fillerForDuplicate.region,
			VPCID: fillerForDuplicate.resourceID, CIDR: fillerForDuplicate.cidr, AssociationID: fillerForDuplicate.associationID,
		}
	}

	var unassumableAcct, neverAttemptedAcct string
	if p.Gapped {
		unassumableAcct = acctID(1000)
		neverAttemptedAcct = acctID(1001)
		facts.Accounts = append(facts.Accounts, unassumableAcct, neverAttemptedAcct)
		facts.UnassumableAccount = unassumableAcct
		facts.NeverAttemptedAccount = neverAttemptedAcct
		failureRows = append(failureRows, [5]string{unassumableAcct, "acct-unassumable", "", "assume-role", "failed"})
	}

	if err := writeNetworksCSV(filepath.Join(outDir, "networks.csv"), rows); err != nil {
		return Facts{}, err
	}
	if err := writeAccountsJSON(filepath.Join(outDir, "accounts.json"), facts.Accounts); err != nil {
		return Facts{}, err
	}
	if err := writeFailuresCSV(filepath.Join(outDir, "failures.csv"), failureRows); err != nil {
		return Facts{}, err
	}
	if err := writeRunJSON(filepath.Join(outDir, "run.json"), accountIDs, runs, unassumableAcct); err != nil {
		return Facts{}, err
	}
	if err := writeReviewedInputs(outDir, &facts); err != nil {
		return Facts{}, err
	}

	return facts, nil
}

func writeNetworksCSV(path string, rows []networkRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	header := []string{"account_id", "account_name", "region", "type", "resource_id", "cidr",
		"parent_id", "az_id", "name", "state", "primary", "association_id", "observed_at"}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		rec := []string{r.accountID, r.accountName, r.region, r.typ, r.resourceID, r.cidr,
			r.parentID, r.azID, r.name, r.state, r.primary, r.associationID, r.observedAt}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writeFailuresCSV(path string, rows [][5]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"account_id", "account_name", "region", "stage", "error"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write(r[:]); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// rawAccountEntry mirrors `aws organizations list-accounts`'s shape exactly
// as scripts/aws/org-inventory.sh writes it verbatim to accounts.json, and
// as internal/onboardcmd/assess.go's own unexported rawAccountsFile decodes
// it -- duplicated here because that type is unexported in package
// onboardcmd.
type rawAccountEntry struct {
	Id     string `json:"Id"`
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

func writeAccountsJSON(path string, accountIDs []string) error {
	entries := make([]rawAccountEntry, len(accountIDs))
	for i, id := range accountIDs {
		entries[i] = rawAccountEntry{Id: id, Name: fmt.Sprintf("acct-%d", i), Status: "ACTIVE"}
	}
	data, err := json.MarshalIndent(struct {
		Accounts []rawAccountEntry `json:"Accounts"`
	}{Accounts: entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// rawRunRegion/rawRunAccount/rawRunFile mirror run.json's shape exactly as
// scripts/aws/org-inventory.sh writes it and
// docs/AWS_ORGANIZATION_INVENTORY.md documents it -- duplicated from
// internal/onboardcmd/assess.go's own unexported types for the same reason
// as rawAccountEntry above.
type rawRunRegion struct {
	Region     string `json:"region"`
	Outcome    string `json:"outcome"`
	Stage      string `json:"stage,omitempty"`
	RowCount   *int   `json:"row_count,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
}

type rawRunAccount struct {
	AccountID          string         `json:"account_id"`
	AccountName        string         `json:"account_name"`
	CredentialSource   string         `json:"credential_source"`
	RegionsAttempted   string         `json:"regions_attempted"`
	NotAttemptedReason *string        `json:"not_attempted_reason"`
	RegionSource       string         `json:"region_source"`
	Regions            []rawRunRegion `json:"regions"`
}

type rawRunFile struct {
	ScriptVersion         string          `json:"script_version"`
	StartedAt             string          `json:"started_at"`
	FinishedAt            string          `json:"finished_at"`
	RoleName              string          `json:"role_name"`
	ManagementAccountUsed bool            `json:"management_account_used"`
	ManagementAccountID   *string         `json:"management_account_id"`
	ConfiguredRegions     []string        `json:"configured_regions"`
	Accounts              []rawRunAccount `json:"accounts"`
}

func writeRunJSON(path string, accountIDs []string, runs regionRuns, unassumableAcct string) error {
	var accounts []rawRunAccount
	for _, a := range accountIDs {
		regions := runs[a]
		if regions == nil {
			continue // the never-attempted account: absent from run.json entirely
		}
		var names []string
		for r := range regions {
			names = append(names, r)
		}
		sort.Strings(names)
		var out []rawRunRegion
		for _, r := range names {
			e := regions[r]
			rc := e.rows
			rr := rawRunRegion{Region: r, Outcome: e.outcome, ObservedAt: observedAt, RowCount: &rc}
			if e.outcome == "partial" {
				rr.Stage = "describe"
			}
			out = append(out, rr)
		}
		accounts = append(accounts, rawRunAccount{
			AccountID: a, AccountName: "acct-" + a, CredentialSource: "assumed-role",
			RegionsAttempted: "known", RegionSource: "configured", Regions: out,
		})
	}
	if unassumableAcct != "" {
		reason := "assume-role-failed"
		accounts = append(accounts, rawRunAccount{
			AccountID: unassumableAcct, AccountName: "acct-unassumable", CredentialSource: "assumed-role",
			RegionsAttempted: "unknown", NotAttemptedReason: &reason, RegionSource: "discovered", Regions: []rawRunRegion{},
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].AccountID < accounts[j].AccountID })

	file := rawRunFile{
		ScriptVersion: "2", StartedAt: observedAt, FinishedAt: observedAt, RoleName: "PlatformIpamReadOnly",
		ManagementAccountUsed: false, ConfiguredRegions: regionPool, Accounts: accounts,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// writeReviewedInputs writes matrix.yaml, ownership.yaml, fixed.yaml and
// decisions.yaml. It plants deterministic impact classifications: when
// EqualCIDR has at least one entry, EqualCIDR[0]'s two accounts are placed
// in two groups the matrix requires to communicate (impact confirmed);
// when Contains has at least one entry, Contains[0]'s two accounts are
// placed in two groups the matrix marks must_stay_isolated (impact
// potential); when EqualCIDR has a second entry, EqualCIDR[1]'s accounts
// are left out of every group entirely (impact unknown). Facts'
// ConfirmedConflictID/IsolatedConflictID/UnknownConflictID are set to match.
func writeReviewedInputs(outDir string, facts *Facts) error {
	var groups, mustCommunicate, mustStayIsolated strings.Builder
	if len(facts.EqualCIDR) > 0 {
		p := facts.EqualCIDR[0]
		fmt.Fprintf(&groups, "  - id: g-equal-a\n    members:\n      - %q\n", p.AccountA)
		fmt.Fprintf(&groups, "  - id: g-equal-b\n    members:\n      - %q\n", p.AccountB)
		mustCommunicate.WriteString("  - [g-equal-a, g-equal-b]\n")
		facts.ConfirmedConflictID = p.ConflictID
	}
	if len(facts.Contains) > 0 {
		p := facts.Contains[0]
		fmt.Fprintf(&groups, "  - id: g-contains-outer\n    members:\n      - %q\n", p.AccountOuter)
		fmt.Fprintf(&groups, "  - id: g-contains-inner\n    members:\n      - %q\n", p.AccountInner)
		mustStayIsolated.WriteString("  - [g-contains-outer, g-contains-inner]\n")
		facts.IsolatedConflictID = p.ConflictID
	}
	if len(facts.EqualCIDR) > 1 {
		facts.UnknownConflictID = facts.EqualCIDR[1].ConflictID
	}

	var doc strings.Builder
	doc.WriteString("# reviewed connectivity matrix -- synthetic, generated by internal/assess/estategen\nversion: 1\ngroups:\n")
	doc.WriteString(groups.String())
	doc.WriteString("must_communicate:\n")
	doc.WriteString(mustCommunicate.String())
	doc.WriteString("must_stay_isolated:\n")
	doc.WriteString(mustStayIsolated.String())
	doc.WriteString("shared_services: []\n")
	if err := os.WriteFile(filepath.Join(outDir, "matrix.yaml"), []byte(doc.String()), 0o644); err != nil {
		return err
	}

	var ownership strings.Builder
	ownership.WriteString("# reviewed ownership table -- synthetic, generated by internal/assess/estategen\n")
	if len(facts.EqualCIDR) > 0 {
		fmt.Fprintf(&ownership, "- account_id: %q\n  product: sample-product\n  environment: prod\n  owner: platform-team\n", facts.EqualCIDR[0].AccountA)
	}
	if err := os.WriteFile(filepath.Join(outDir, "ownership.yaml"), []byte(ownership.String()), 0o644); err != nil {
		return err
	}

	fixed := "# fixed (non-AWS) ranges -- synthetic, generated by internal/assess/estategen\n" +
		"- cidr: 192.0.2.0/24\n  description: on-premises documentation range (RFC 5737)\n  owner: network-team\n"
	if err := os.WriteFile(filepath.Join(outDir, "fixed.yaml"), []byte(fixed), 0o644); err != nil {
		return err
	}

	var decisions strings.Builder
	decisions.WriteString("# decisions -- synthetic, generated by internal/assess/estategen\n")
	if facts.ConfirmedConflictID != "" {
		fmt.Fprintf(&decisions, "%s:\n  decision: accepted-risk\n  remediation: renumber one VPC before the migration wave\n  responsible: network-team\n  reviewed_by: estategen\n  reviewed_at: %q\n",
			facts.ConfirmedConflictID, observedAt)
	}
	if err := os.WriteFile(filepath.Join(outDir, "decisions.yaml"), []byte(decisions.String()), 0o644); err != nil {
		return err
	}
	return nil
}
