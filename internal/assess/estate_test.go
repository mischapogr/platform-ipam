package assess

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
)

// estateParams configures genEstate, this package's own small synthetic
// estate generator. Package M1b4 builds the shared generator the work plan
// names; this one exists only so M1b2's own tests (determinism,
// traceability, the coverage table, and the scale bound) do not each hand-
// write fixtures, and it stays unexported.
type estateParams struct {
	accounts             int
	regionsPerAccount    int
	vpcsPerAccountRegion int
	subnetsPerVPC        int
	// conflictFraction is the fraction of VPCs whose CIDR is drawn from a
	// small shared pool instead of a unique block, so the estate contains a
	// deliberate, predictable number of equal-cidr conflicts.
	conflictFraction float64
	// unreadAccounts is how many trailing accounts fail assume-role
	// entirely (no records, a failures.csv row, no run.json attempt).
	unreadAccounts int
	// partialRegions is how many (account, region) pairs get both VPC rows
	// and a failed "describe" row for the same pair.
	partialRegions int
	// includeRun controls whether a RunRecord is attached at all.
	includeRun bool
	seed       int64
}

func genEstate(p estateParams) Input {
	rng := rand.New(rand.NewSource(p.seed))
	var records []ResourceRecord
	var accounts []AccountRecord
	var failures []FailureRow
	var attempts []RunAttempt
	row := 0

	conflictPoolSize := 5
	obsAt := "2026-09-20T00:00:00Z"

	for a := 0; a < p.accounts; a++ {
		acctID := fmt.Sprintf("%012d", 100000000000+int64(a))
		accounts = append(accounts, AccountRecord{AccountID: acctID, Name: fmt.Sprintf("acct-%d", a), Status: "ACTIVE"})

		if a < p.unreadAccounts {
			row++
			failures = append(failures, FailureRow{AccountID: acctID, AccountName: fmt.Sprintf("acct-%d", a), Stage: StageAssumeRole, Error: "AccessDenied", SourceFile: "failures.csv", SourceRow: row})
			continue // no records, no attempts: an entirely unread account
		}

		for r := 0; r < p.regionsPerAccount; r++ {
			region := fmt.Sprintf("region-%d", r)
			partial := a >= p.unreadAccounts && (a-p.unreadAccounts)*p.regionsPerAccount+r < p.partialRegions

			for v := 0; v < p.vpcsPerAccountRegion; v++ {
				vpcID := fmt.Sprintf("vpc-%d-%d-%d", a, r, v)
				var vpcCIDR netip.Prefix
				if rng.Float64() < p.conflictFraction {
					pool := rng.Intn(conflictPoolSize)
					vpcCIDR = netip.MustParsePrefix(fmt.Sprintf("10.%d.0.0/20", pool))
				} else {
					idx := a*10007 + r*97 + v // large odd multipliers spread indices out
					vpcCIDR = uniquePrefix(idx, 20)
				}
				row++
				assoc := fmt.Sprintf("eni-assoc-%d", row)
				primary := TriTrue
				records = append(records, ResourceRecord{
					AccountID: acctID, Region: region, Type: TypeVPC, ResourceID: vpcID, CIDR: vpcCIDR.String(),
					AssociationID: &assoc, Primary: primary, ObservedAt: &obsAt,
					SourceFile: "networks.csv", SourceRow: row,
				})

				for s := 0; s < p.subnetsPerVPC && s < 16; s++ {
					subnetCIDR := subnetOf(vpcCIDR, s, 4)
					row++
					records = append(records, ResourceRecord{
						AccountID: acctID, Region: region, Type: TypeSubnet, ResourceID: fmt.Sprintf("subnet-%d", row), CIDR: subnetCIDR.String(), ParentID: vpcID,
						SourceFile: "networks.csv", SourceRow: row,
					})
				}
			}

			if partial {
				row++
				failures = append(failures, FailureRow{AccountID: acctID, AccountName: fmt.Sprintf("acct-%d", a), Region: region, Stage: StageDescribe, Error: "RequestLimitExceeded", SourceFile: "failures.csv", SourceRow: row})
			}
			attempts = append(attempts, RunAttempt{AccountID: acctID, Region: region, Outcome: AttemptSucceeded})
		}
	}

	in := Input{Records: records, Accounts: accounts, Failures: failures}
	if p.includeRun {
		in.Run = &RunRecord{StartedAt: "2026-09-20T00:00:00Z", FinishedAt: "2026-09-20T00:05:00Z", RoleName: "PlatformIpamReadOnly", Attempts: attempts, SourceFile: "run.json"}
	}
	return in
}

// uniquePrefix derives a deterministic, collision-free /bits IPv4 prefix
// from an integer index, walking the 10.0.0.0/8 space in blocks of the
// requested size.
func uniquePrefix(index, bits int) netip.Prefix {
	blockSize := uint32(1) << uint(32-bits)
	base := uint32(10) << 24
	addr := base + uint32(index)*blockSize
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], addr)
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
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
	base := binary.BigEndian.Uint32(a4[:])
	addr := base + uint32(index)*step
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], addr)
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
}
