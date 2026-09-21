package adoptcmd

// orderParentsFirst returns records with every vpc-scope row before every
// subnet-scope row, each group keeping its original relative order
// (docs/WORK_PLAN.md package F4: "parents (vpc) before subnets regardless of
// input order"). A record whose scope is neither -- already destined for a
// VerdictRefused from requestFor -- is left in the subnet group; ordering
// never decides whether a record is valid, only when it is attempted.
func orderParentsFirst(records []Record) []Record {
	vpcs := make([]Record, 0, len(records))
	subnets := make([]Record, 0, len(records))
	for _, r := range records {
		if r.Scope == "vpc" {
			vpcs = append(vpcs, r)
		} else {
			subnets = append(subnets, r)
		}
	}
	return append(vpcs, subnets...)
}
