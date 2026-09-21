package assess

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Group is one connectivity group of the reviewed matrix: an id and the
// member entries ADR 0014 defines -- an account id, a VPC id or a product
// name, matched in that order of precedence.
type Group struct {
	ID      string   `json:"id"`
	Members []string `json:"members"`
}

// Matrix is the reviewed, customer-supplied connectivity matrix, gap M2's
// minimal format (ADR 0014, "Impact is three-valued"). DecodeMatrix is the
// only way to build one from outside this package.
//
// The record calls this "one YAML document". This package may import
// nothing beyond the standard library (see TestImportsAreStandardLibraryOnly),
// and the standard library has no YAML decoder, so DecodeMatrix accepts the
// JSON encoding of exactly this schema instead -- every valid JSON document
// is already valid YAML 1.2, so a caller who does own a YAML library can
// decode a YAML file into this same shape and re-encode it as JSON before
// handing it to this package. This is a decision the record did not make;
// see the package-level report to the lead.
type Matrix struct {
	Version          int         `json:"version"`
	Groups           []Group     `json:"groups"`
	MustCommunicate  [][2]string `json:"must_communicate"`
	MustStayIsolated [][2]string `json:"must_stay_isolated"`
	SharedServices   []string    `json:"shared_services"`
}

// DecodeMatrix decodes r as a Matrix. It performs no I/O beyond reading r.
func DecodeMatrix(r io.Reader) (Matrix, error) {
	var m Matrix
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Matrix{}, fmt.Errorf("decoding connectivity matrix: %w", err)
	}
	return m, nil
}

// OwnershipEntry is one row of the --ownership table.
type OwnershipEntry struct {
	AccountID   string `json:"account_id"`
	VPCID       string `json:"vpc_id,omitempty"` // empty means the whole account
	Product     string `json:"product"`
	Environment string `json:"environment"`
	Owner       string `json:"owner"`
}

// Ownership is the decoded --ownership table.
type Ownership struct {
	Entries []OwnershipEntry `json:"entries"`
}

// DecodeOwnership decodes r as an Ownership table (a JSON array of
// OwnershipEntry, or an object with an "entries" array -- both accepted so a
// caller may choose whichever shape a spreadsheet export produces more
// naturally).
func DecodeOwnership(r io.Reader) (Ownership, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Ownership{}, fmt.Errorf("reading ownership table: %w", err)
	}
	var entries []OwnershipEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return Ownership{Entries: entries}, nil
	}
	var wrapped Ownership
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return Ownership{}, fmt.Errorf("decoding ownership table: %w", err)
	}
	return wrapped, nil
}

// lookup returns the most specific matching entry for an account/VPC: an
// entry naming this exact VPC id wins over one naming only the account
// (empty vpc_id).
func (o Ownership) lookup(accountID, vpcID string) (OwnershipEntry, bool) {
	var accountMatch OwnershipEntry
	haveAccountMatch := false
	for _, e := range o.Entries {
		if e.AccountID != accountID {
			continue
		}
		if e.VPCID == vpcID && vpcID != "" {
			return e, true
		}
		if e.VPCID == "" {
			accountMatch = e
			haveAccountMatch = true
		}
	}
	return accountMatch, haveAccountMatch
}

const unknownOwnership = "unknown"

// resolve returns product, environment, owner for a resource: the literal
// string "unknown" in every field with no matching entry, never an empty
// string (ADR 0014, "Ownership").
func (o Ownership) resolve(accountID, vpcID string) (product, environment, owner string) {
	e, ok := o.lookup(accountID, vpcID)
	if !ok {
		return unknownOwnership, unknownOwnership, unknownOwnership
	}
	product, environment, owner = e.Product, e.Environment, e.Owner
	if product == "" {
		product = unknownOwnership
	}
	if environment == "" {
		environment = unknownOwnership
	}
	if owner == "" {
		owner = unknownOwnership
	}
	return product, environment, owner
}

// DecodeFixed decodes r as a []FixedRange (a JSON array).
func DecodeFixed(r io.Reader) ([]FixedRange, error) {
	var rows []FixedRange
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("decoding fixed-range table: %w", err)
	}
	return rows, nil
}

// Decision is one entry of the --decisions file, keyed by conflict id.
type Decision struct {
	Decision    string `json:"decision"`
	Remediation string `json:"remediation,omitempty"`
	Responsible string `json:"responsible,omitempty"`
	ReviewedBy  string `json:"reviewed_by,omitempty"`
	ReviewedAt  string `json:"reviewed_at,omitempty"`
}

// DecodeDecisions decodes r as a map of conflict id to Decision.
func DecodeDecisions(r io.Reader) (map[string]Decision, error) {
	var m map[string]Decision
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding decisions file: %w", err)
	}
	return m, nil
}

// groupAssignment is the result of matching one VPC against the matrix's
// groups: the matched group id, or ambiguous if more than one group's
// members matched.
type groupAssignment struct {
	groupID   string
	assigned  bool
	ambiguous bool
}

// assignGroups resolves every VPC identity present in items to a matrix
// group, following the precedence ADR 0014 states: an explicit VPC id wins
// over a product, and a product wins over an account. A VPC matched by more
// than one group (after applying precedence -- i.e. more than one group
// claims it at the winning precedence level) is ambiguous, and the matrix is
// reported as ambiguous for that VPC; every conflict touching it on either
// side is unknown.
func assignGroups(m Matrix, ownership Ownership, vpcKeys map[string]vpcIdentity) map[string]groupAssignment {
	out := map[string]groupAssignment{}
	for key, v := range vpcKeys {
		out[key] = matchVPCGroup(m, ownership, v)
	}
	return out
}

// vpcIdentity is the account/region/VPC-id/product a matrix match needs.
type vpcIdentity struct {
	accountID string
	vpcID     string
	product   string
}

func matchVPCGroup(m Matrix, ownership Ownership, v vpcIdentity) groupAssignment {
	// Precedence level 1: explicit VPC id.
	if ids := matchingGroups(m, func(member string) bool { return member == v.vpcID }); len(ids) > 0 {
		return resolveMatches(ids)
	}
	// Precedence level 2: product name (resolved through the ownership
	// table, never through the AWS "Name" tag or account naming).
	product, _, _ := ownership.resolve(v.accountID, v.vpcID)
	if product != "" && product != unknownOwnership {
		if ids := matchingGroups(m, func(member string) bool { return member == product }); len(ids) > 0 {
			return resolveMatches(ids)
		}
	}
	// Precedence level 3: account id.
	if ids := matchingGroups(m, func(member string) bool { return member == v.accountID }); len(ids) > 0 {
		return resolveMatches(ids)
	}
	return groupAssignment{}
}

func matchingGroups(m Matrix, match func(member string) bool) []string {
	var ids []string
	for _, g := range m.Groups {
		for _, member := range g.Members {
			if match(member) {
				ids = append(ids, g.ID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func resolveMatches(ids []string) groupAssignment {
	if len(ids) == 1 {
		return groupAssignment{groupID: ids[0], assigned: true}
	}
	return groupAssignment{ambiguous: true}
}

// matrixRelation returns the raw relation the matrix expresses between two
// groups (ADR 0014: "must_communicate, must_stay_isolated, undecided or
// not_in_matrix"). Either side unassigned (including "no matrix at all",
// which assignGroups never assigns anything for) is RelationNotInMatrix --
// the pair cannot be located in either list without both groups.
func matrixRelation(m Matrix, groupA, groupB string, assignedA, assignedB bool) MatrixRelation {
	if !assignedA || !assignedB {
		return RelationNotInMatrix
	}
	if groupA == groupB {
		// A group is by definition a set whose members must communicate.
		return RelationMustCommunicate
	}
	if isSharedService(m, groupA) || isSharedService(m, groupB) {
		return RelationMustCommunicate
	}
	if pairListed(m.MustCommunicate, groupA, groupB) {
		return RelationMustCommunicate
	}
	if pairListed(m.MustStayIsolated, groupA, groupB) {
		return RelationMustStayIsolated
	}
	return RelationUndecided
}

func isSharedService(m Matrix, groupID string) bool {
	for _, id := range m.SharedServices {
		if id == groupID {
			return true
		}
	}
	return false
}

func pairListed(pairs [][2]string, a, b string) bool {
	for _, p := range pairs {
		if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
			return true
		}
	}
	return false
}

// impactFor derives the three-valued Impact from a MatrixRelation and
// whether the matrix was supplied at all (ADR 0014, "Impact is
// three-valued"):
//
//	no matrix, or neither side assigned  -> unknown
//	must_communicate                     -> confirmed
//	must_stay_isolated or undecided,
//	  with at least one side assigned    -> potential
//	not_in_matrix                        -> unknown
func impactFor(hasMatrix bool, relation MatrixRelation, assignedA, assignedB bool) Impact {
	if !hasMatrix {
		return ImpactUnknown
	}
	switch relation {
	case RelationMustCommunicate:
		return ImpactConfirmed
	case RelationMustStayIsolated, RelationUndecided:
		return ImpactPotential
	default: // RelationNotInMatrix
		if assignedA || assignedB {
			return ImpactPotential
		}
		return ImpactUnknown
	}
}
