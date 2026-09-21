package assess

import (
	"fmt"
	"sort"
)

// MatrixRelation is the raw relation the connectivity matrix expressed for a
// conflict's two sides, carried beside Impact so "no judgement is hidden
// inside a single word" (ADR 0014).
type MatrixRelation string

const (
	RelationMustCommunicate  MatrixRelation = "must_communicate"
	RelationMustStayIsolated MatrixRelation = "must_stay_isolated"
	RelationUndecided        MatrixRelation = "undecided"
	RelationNotInMatrix      MatrixRelation = "not_in_matrix"
)

// Impact is the three-valued classification ADR 0014 requires: a CIDR
// collision is always a fact (a Conflict is reported regardless of Impact);
// whether it blocks a required connection is this field, and it is never
// derived from anything but a supplied Matrix.
type Impact string

const (
	ImpactConfirmed Impact = "confirmed"
	ImpactPotential Impact = "potential"
	ImpactUnknown   Impact = "unknown"
)

// Side is one resource of a Conflict.
type Side struct {
	Identity      string  `json:"identity"`
	AccountID     string  `json:"account_id,omitempty"`
	AccountName   string  `json:"account_name,omitempty"`
	Region        string  `json:"region,omitempty"`
	VPCID         string  `json:"vpc_id,omitempty"`
	CIDR          string  `json:"cidr"`
	AssociationID *string `json:"association_id"`
	Primary       Tri     `json:"primary"`
	Name          string  `json:"name,omitempty"`
	State         string  `json:"state,omitempty"`
	ObservedAt    *string `json:"observed_at"`
	Product       string  `json:"product"`
	Environment   string  `json:"environment"`
	Owner         string  `json:"owner"`
	// Group is the connectivity group this side was assigned to, empty when
	// unassigned or ambiguous.
	Group     string `json:"group,omitempty"`
	Ambiguous bool   `json:"group_ambiguous,omitempty"`
	// Position is "outer"/"inner" for a KindContains Conflict, empty
	// otherwise. The Go field is named Position, not Role, because
	// internal/config/config_test.go's TestOperatorRoleOnlyConstructedByConfigLoad
	// scans the whole cmd/ and internal/ tree for any composite-literal key
	// or field named Role outside config.Load's own decode path -- a
	// repository-wide guard against Principal.Role being set from anywhere
	// but a trusted YAML load. This field has nothing to do with
	// domain.Principal; it still renders as "role" in JSON, which is the
	// word ADR 0014 uses.
	Position Position `json:"role,omitempty"`
	// Fixed marks a side that came from the --fixed table rather than an
	// observed AWS resource.
	Fixed      bool   `json:"fixed"`
	SourceFile string `json:"source_file"`
	SourceRow  int    `json:"source_row"`
}

// Conflict is one equal-cidr or contains relationship between two
// associations of different VPCs (or one VPC association and one --fixed
// range).
type Conflict struct {
	ID             string         `json:"id"`
	Kind           Kind           `json:"kind"`
	Impact         Impact         `json:"impact"`
	MatrixRelation MatrixRelation `json:"matrix_relation"`
	Intersection   string         `json:"intersection"`
	Fixed          bool           `json:"fixed"`
	// Sides always has exactly two entries, sorted by Identity ascending so
	// the ordering is independent of which side the sweep happened to visit
	// first.
	Sides []Side `json:"sides"`
	// IsolationNote is set only when MatrixRelation is
	// RelationMustStayIsolated: isolation is a policy intention, not a
	// property of the addresses (ADR 0014, "Impact is three-valued").
	IsolationNote string `json:"isolation_note,omitempty"`
	// Decision is this conflict's entry from --decisions, if any, echoed in
	// without changing anything above it.
	Decision *Decision `json:"decision,omitempty"`
}

func positionFor(kind Kind, outer bool) Position {
	if kind != KindContains {
		return ""
	}
	if outer {
		return PositionOuter
	}
	return PositionInner
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func sideFrom(a association, role Position) Side {
	if a.fixed {
		return Side{
			Identity:    a.identity,
			CIDR:        a.fx.CIDR,
			Product:     unknownOwnership,
			Environment: unknownOwnership,
			Owner:       valueOr(a.fx.Owner, unknownOwnership),
			Name:        a.fx.Description,
			Position:    role,
			Fixed:       true,
			SourceFile:  a.fx.SourceFile,
			SourceRow:   a.fx.SourceRow,
		}
	}
	r := a.rec
	return Side{
		Identity:      r.Identity(),
		AccountID:     r.AccountID,
		AccountName:   r.AccountName,
		Region:        r.Region,
		VPCID:         r.ResourceID,
		CIDR:          r.CIDR,
		AssociationID: r.AssociationID,
		Primary:       r.primary(),
		Name:          r.Name,
		State:         r.State,
		ObservedAt:    r.ObservedAt,
		Position:      role,
		Fixed:         false,
		SourceFile:    r.SourceFile,
		SourceRow:     r.SourceRow,
	}
}

// buildConflicts turns the sweep's raw pairs into Conflicts: ownership,
// connectivity-group assignment, matrix relation, impact, a stable id and
// any decision on file. It also returns one Note per VPC the matrix could
// not assign unambiguously.
func buildConflicts(rels []rawRelationship, ownership Ownership, matrix Matrix, hasMatrix bool, decisions map[string]Decision) ([]Conflict, []Note) {
	vpcKeys := map[string]vpcIdentity{}
	note := func(a association) {
		if a.fixed {
			return
		}
		key := a.rec.vpcKey()
		if _, ok := vpcKeys[key]; ok {
			return
		}
		product, _, _ := ownership.resolve(a.rec.AccountID, a.rec.ResourceID)
		vpcKeys[key] = vpcIdentity{accountID: a.rec.AccountID, vpcID: a.rec.ResourceID, product: product}
	}
	for _, rel := range rels {
		note(rel.x)
		note(rel.y)
	}
	assignments := assignGroups(matrix, ownership, vpcKeys)

	var ambiguousKeys []string
	for key, asg := range assignments {
		if asg.ambiguous {
			ambiguousKeys = append(ambiguousKeys, key)
		}
	}
	sort.Strings(ambiguousKeys)
	var notes []Note
	for _, key := range ambiguousKeys {
		v := vpcKeys[key]
		notes = append(notes, Note{Kind: NoteAmbiguousGroup,
			Message: fmt.Sprintf("VPC %s in account %s matches more than one connectivity group; every conflict touching it on either side is impact unknown", v.vpcID, v.accountID)})
	}

	assignSide := func(a association, side *Side) (groupID string, assigned bool) {
		if a.fixed {
			return "", false
		}
		key := a.rec.vpcKey()
		product, env, owner := ownership.resolve(a.rec.AccountID, a.rec.ResourceID)
		side.Product, side.Environment, side.Owner = product, env, owner
		asg := assignments[key]
		if asg.ambiguous {
			side.Ambiguous = true
			return "", false
		}
		if asg.assigned {
			side.Group = asg.groupID
			return asg.groupID, true
		}
		return "", false
	}

	var conflicts []Conflict
	for _, rel := range rels {
		sideX := sideFrom(rel.x, positionFor(rel.kind, true))
		sideY := sideFrom(rel.y, positionFor(rel.kind, false))
		groupX, assignedX := assignSide(rel.x, &sideX)
		groupY, assignedY := assignSide(rel.y, &sideY)

		relation := matrixRelation(matrix, groupX, groupY, assignedX, assignedY)
		impact := impactFor(hasMatrix, relation, assignedX, assignedY)

		var isolationNote string
		if relation == RelationMustStayIsolated {
			isolationNote = "the matrix marks these groups must_stay_isolated; isolation is a policy intention enforced by route tables and security controls, not a property of the addresses, and the overlapping ranges make a later change of that decision expensive"
		}

		id := ConflictID(rel.kind, rel.x.identity, rel.y.identity)
		sides := []Side{sideX, sideY}
		sort.Slice(sides, func(i, j int) bool { return sides[i].Identity < sides[j].Identity })

		c := Conflict{
			ID:             id,
			Kind:           rel.kind,
			Impact:         impact,
			MatrixRelation: relation,
			Intersection:   cidrString(rel.y),
			Fixed:          rel.x.fixed || rel.y.fixed,
			Sides:          sides,
			IsolationNote:  isolationNote,
		}
		if d, ok := decisions[id]; ok {
			dCopy := d
			c.Decision = &dCopy
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, notes
}

// sortConflicts orders conflicts by kind, then by the first resource
// identity, then by the second, then by the id -- a total order with no
// ties (ADR 0014, "Identity and determinism").
func sortConflicts(conflicts []Conflict) {
	sort.SliceStable(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Sides[0].Identity != b.Sides[0].Identity {
			return a.Sides[0].Identity < b.Sides[0].Identity
		}
		if a.Sides[1].Identity != b.Sides[1].Identity {
			return a.Sides[1].Identity < b.Sides[1].Identity
		}
		return a.ID < b.ID
	})
}
