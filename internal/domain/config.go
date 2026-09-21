package domain

type Config struct {
	SchemaVersion  int            `yaml:"schema_version"`
	PolicyVersion  string         `yaml:"policy_version"`
	Domains        []Domain       `yaml:"overlap_domains"`
	Pools          []Pool         `yaml:"pools"`
	SubnetPolicy   SubnetPolicy   `yaml:"subnet_policy"`
	Lifecycle      Lifecycle      `yaml:"lifecycle"`
	Reconciliation Reconciliation `yaml:"reconciliation"`
	UI             UI             `yaml:"ui"`
	Identities     []Principal    `yaml:"identities"`
}
type Backend struct {
	Type                  string `yaml:"type"`
	VRFID                 int    `yaml:"vrf_id"`
	RequireUniquePrefixes bool   `yaml:"require_unique_prefixes"`
	ParentPrefixID        int    `yaml:"parent_prefix_id"`
}
type Domain struct {
	ID                 string  `yaml:"id"`
	Description        string  `yaml:"description"`
	CoverageGeneration string  `yaml:"coverage_generation"`
	Backend            Backend `yaml:"inventory_backend"`
	CloudCoverage      []Cell  `yaml:"cloud_coverage"`
}
type Cell struct {
	AccountID string   `yaml:"account_id"`
	RoleARN   string   `yaml:"role_arn"`
	Regions   []string `yaml:"regions"`
}
type Pool struct {
	ID                   string   `yaml:"id"`
	DomainID             string   `yaml:"overlap_domain"`
	CIDR                 string   `yaml:"cidr"`
	AddressFamily        string   `yaml:"address_family"`
	Scope                string   `yaml:"scope"`
	Environment          string   `yaml:"environment"`
	Region               string   `yaml:"region"`
	EligibleTenants      []string `yaml:"eligible_tenants"`
	EligibleAccounts     []string `yaml:"eligible_accounts"`
	AllowedPrefixLengths []int    `yaml:"allowed_prefix_lengths"`
	ExcludedCIDRs        []string `yaml:"excluded_cidrs"`
	MaxAllocations       int      `yaml:"max_unreclaimed_allocations_per_tenant"`
	Backend              Backend  `yaml:"inventory_backend"`
}
type SubnetPolicy struct {
	RequireParentScope   string `yaml:"require_parent_scope"`
	InheritParentTarget  bool   `yaml:"inherit_parent_target"`
	AllowedPrefixLengths []int  `yaml:"allowed_prefix_lengths"`
	MaxChildren          int    `yaml:"max_unreclaimed_children"`
	AllowNestedSubnets   bool   `yaml:"allow_nested_subnets"`
}
type Lifecycle struct {
	ReservationExpiryEnabled bool `yaml:"reservation_expiry_enabled"`
	ReservationAgeAlertHours int  `yaml:"reservation_age_alert_hours"`
	QuarantineHours          int  `yaml:"quarantine_hours"`
	RequiredAbsenceScans     int  `yaml:"required_complete_absence_scans"`
	MinScanSpacing           int  `yaml:"minimum_scan_spacing_seconds"`
	MaxObservationAge        int  `yaml:"maximum_observation_age_seconds"`
}
type Reconciliation struct {
	FullScanInterval    int  `yaml:"full_scan_interval_seconds"`
	IncludeUntagged     bool `yaml:"include_untagged_resources"`
	IncludeAssociations bool `yaml:"include_all_vpc_cidr_associations"`
	UnknownBlocksReuse  bool `yaml:"unknown_coverage_blocks_reuse"`
}
type UI struct {
	InventoryLinksEnabled bool   `yaml:"inventory_links_enabled"`
	NetBoxBaseURL         string `yaml:"netbox_base_url"`
}
