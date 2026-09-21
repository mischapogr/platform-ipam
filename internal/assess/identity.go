package assess

import (
	"crypto/sha256"
	"encoding/hex"
)

// Kind is one of the two relationships ADR 0014 defines. There is no third:
// a CIDR block of prefix length n is an interval of 2^(32-n) addresses
// aligned to a multiple of its own size, so two blocks that share any
// address are either identical or one lies wholly inside the other --
// partial overlap between CIDR blocks is impossible. See
// TestCIDRBlocksNeverPartiallyOverlap for the property test.
type Kind string

const (
	KindEqualCIDR Kind = "equal-cidr"
	KindContains  Kind = "contains"
)

// Position labels a Side of a KindContains Conflict. It is empty for
// KindEqualCIDR, where neither side is more "outer" than the other.
type Position string

const (
	PositionOuter Position = "outer"
	PositionInner Position = "inner"
)

// ConflictID returns "c-" followed by the first sixteen hex characters of
// the SHA-256 of the relationship kind and the two resource identities,
// after sorting the two identities as byte strings (ADR 0014, "Identity and
// determinism"). Sorting before hashing makes the id independent of which
// record was read first, so it is order-independent: ConflictID(k, a, b) ==
// ConflictID(k, b, a).
//
// The id deliberately excludes everything but the relationship kind and the
// two resource identities -- not the observation time, the file, the row,
// ownership, the matrix or the impact -- so a decision recorded against an
// id survives a re-collection, a newly supplied matrix, and a growing
// inventory; only a change to one of the two resources themselves changes
// the id.
func ConflictID(kind Kind, identityA, identityB string) string {
	if identityB < identityA {
		identityA, identityB = identityB, identityA
	}
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + identityA + "\x00" + identityB))
	return "c-" + hex.EncodeToString(sum[:])[:16]
}
