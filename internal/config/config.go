package config

import (
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"go.yaml.in/yaml/v3"
)

type Settings struct {
	UIInventoryLinks, UINetBoxURL                                                                                 string
	Environment, DatabaseURL, ConfigFile, IdentityFile, AuthMode, LocalToken, LocalSubject, LocalExtraCredentials string
	NetBoxURL, NetBoxToken, AWSMode, FakeCloudFile, ListenAddr, OIDCIssuer, OIDCAudience                          string
}

func Environment() Settings {
	get := func(key, fallback string) string {
		if s := os.Getenv("IPAM_" + key); s != "" {
			return s
		}
		return fallback
	}
	return Settings{Environment: get("ENVIRONMENT", "development"), DatabaseURL: get("DATABASE_URL", ""), ConfigFile: get("CONFIG_FILE", "examples/config/pools.yaml"), IdentityFile: get("IDENTITY_FILE", ""), AuthMode: get("AUTH_MODE", "local"), LocalToken: get("LOCAL_TOKEN", ""), LocalSubject: get("LOCAL_SUBJECT", "developer"), LocalExtraCredentials: get("LOCAL_EXTRA_CREDENTIALS", ""), NetBoxURL: get("NETBOX_URL", ""), NetBoxToken: get("NETBOX_TOKEN", ""), AWSMode: get("AWS_MODE", "fake"), FakeCloudFile: get("FAKE_CLOUD_FILE", ""), ListenAddr: get("LISTEN_ADDR", ":8080"), OIDCIssuer: get("OIDC_ISSUER", ""), OIDCAudience: get("OIDC_AUDIENCE", ""), UIInventoryLinks: get("UI_INVENTORY_LINKS_ENABLED", ""), UINetBoxURL: get("UI_NETBOX_BASE_URL", "")}
}
func (s Settings) Validate(mode string) error {
	if !slices.Contains([]string{"development", "stage", "prod"}, s.Environment) {
		return fmt.Errorf("IPAM_ENVIRONMENT must be development, stage, or prod")
	}
	if s.DatabaseURL == "" {
		return fmt.Errorf("IPAM_DATABASE_URL is required")
	}
	if s.Environment != "development" {
		u, err := url.Parse(s.DatabaseURL)
		if err != nil || u.Query().Get("sslmode") != "verify-full" {
			return fmt.Errorf("stage/prod database URL requires sslmode=verify-full")
		}
	}
	if mode == "migrate" {
		return nil
	}
	// adopt (docs/WORK_PLAN.md package H5, found by H3's chart review)
	// authenticates no caller and answers no HTTP request at all --
	// cmd/platform-ipam/main.go dispatches it, runs its subcommand and exits
	// before ever reaching the block that builds transport.NewAuth or the UI
	// links an api response renders from these two settings -- so unlike
	// every other non-migrate mode it validates neither the UI settings nor
	// even that IPAM_AUTH_MODE names a real mode; nothing downstream in this
	// function ever reads s.AuthMode for adopt. worker starts an HTTP server
	// too (for /livez and /readyz), so it keeps this enum check and the UI
	// settings below unlike adopt; what it shares with adopt is investigated
	// further down, at the stage/prod split and the OIDC issuer/audience
	// check, because neither mode's server ever authenticates anything.
	if mode != "adopt" {
		if s.UIInventoryLinks != "" {
			if _, err := strconv.ParseBool(s.UIInventoryLinks); err != nil {
				return fmt.Errorf("IPAM_UI_INVENTORY_LINKS_ENABLED must be a boolean")
			}
		}
		if s.UINetBoxURL != "" {
			if err := secureOrigin(s.UINetBoxURL, s.Environment == "development"); err != nil {
				return fmt.Errorf("IPAM_UI_NETBOX_BASE_URL must be a valid browser origin")
			}
		}
		if s.AuthMode != "local" && s.AuthMode != "oidc" {
			return fmt.Errorf("IPAM_AUTH_MODE must be local or oidc")
		}
	}
	// A second guard against the same mistake NewAuth refuses at construction
	// time (package G3c): IPAM_LOCAL_EXTRA_CREDENTIALS must never be honoured
	// outside development, so settings validation fails closed on it here too,
	// independent of IPAM_AUTH_MODE and of mode (adopt included), rather than
	// relying on NewAuth alone to catch it.
	if s.LocalExtraCredentials != "" && s.Environment != "development" {
		return fmt.Errorf("IPAM_LOCAL_EXTRA_CREDENTIALS is permitted only when IPAM_ENVIRONMENT is development")
	}
	if s.AWSMode != "fake" && s.AWSMode != "live" {
		return fmt.Errorf("IPAM_AWS_MODE must be fake or live")
	}
	if s.Environment != "development" {
		// Live AWS observations are required in stage/prod for every
		// non-migrate mode that can build a cloud observer or serve reads
		// backed by one -- api, worker and adopt alike -- so this half of the
		// old combined rule stays unconditional on mode.
		if s.AWSMode != "live" {
			return fmt.Errorf("stage/prod require live AWS observations")
		}
		// OIDC authentication is required in stage/prod only for the one mode
		// that ever authenticates a caller: cmd/platform-ipam/main.go
		// constructs transport.NewAuth for mode == "api" alone. adopt is the
		// case package H5 was asked to fix; worker turned out to share the
		// exact same quirk (package H5's own evidence: on the unmodified
		// code, Settings{Environment: "prod", AuthMode: "local", AWSMode:
		// "live", ...}.Validate("worker") refused for the same reason
		// Validate("adopt") did) and is fixed the same way here.
		if mode == "api" && s.AuthMode != "oidc" {
			return fmt.Errorf("stage/prod require OIDC authentication")
		}
	}
	if s.AuthMode == "local" && mode == "api" && len(s.LocalToken) < 24 {
		return fmt.Errorf("development API requires IPAM_LOCAL_TOKEN of at least 24 characters")
	}
	// OIDC issuer/audience are read only by transport.NewAuth's oidc branch,
	// which -- like NewAuth itself -- is only ever constructed for mode ==
	// "api" (cmd/platform-ipam/main.go). Gating this on mode, not just on
	// s.AuthMode == "oidc", stops a value inherited from another process's
	// environment (the Helm operatorJob currently hands "adopt" the worker's
	// whole environment, IPAM_AUTH_MODE included) from demanding settings
	// that mode will never read.
	if mode == "api" && s.AuthMode == "oidc" {
		if err := secureOrigin(s.OIDCIssuer, false); err != nil || s.OIDCAudience == "" {
			return fmt.Errorf("OIDC requires HTTPS issuer and explicit API audience")
		}
	}
	if err := secureOrigin(s.NetBoxURL, s.Environment == "development"); err != nil {
		return fmt.Errorf("IPAM_NETBOX_URL: %w", err)
	}
	if s.NetBoxToken == "" {
		return fmt.Errorf("IPAM_NETBOX_TOKEN is required")
	}
	if s.AWSMode == "fake" && s.FakeCloudFile == "" {
		return fmt.Errorf("fake observation requires IPAM_FAKE_CLOUD_FILE")
	}
	return nil
}
func secureOrigin(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return fmt.Errorf("expected a credential-free HTTPS URL")
	}
	return nil
}

func Load(path, identityFile, environment string) (domain.Config, error) {
	var cfg domain.Config
	if err := decode(path, &cfg); err != nil {
		return cfg, err
	}
	if identityFile != "" {
		var entries struct {
			Identities []domain.Principal `yaml:"identities"`
		}
		if err := decode(identityFile, &entries); err != nil {
			return cfg, err
		}
		cfg.Identities = entries.Identities
	}
	return cfg, Validate(cfg, environment)
}
func decode(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 2<<20))
	d.KnownFields(true)
	if err = d.Decode(target); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("configuration must contain one YAML document")
	}
	return nil
}

var accountPattern = regexp.MustCompile(`^[0-9]{12}$`)
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func Validate(c domain.Config, environment string) error {
	if c.UI.InventoryLinksEnabled {
		if err := secureOrigin(c.UI.NetBoxBaseURL, environment == "development"); err != nil {
			return fmt.Errorf("enabled inventory links require ui.netbox_base_url")
		}
	}
	if c.SchemaVersion != 1 || c.PolicyVersion == "" || len(c.Domains) == 0 || len(c.Pools) == 0 {
		return fmt.Errorf("schema_version=1, policy_version, domains and pools are required")
	}
	if c.Lifecycle.ReservationExpiryEnabled || c.Lifecycle.QuarantineHours < 0 || c.Lifecycle.RequiredAbsenceScans < 2 || c.Lifecycle.MinScanSpacing < 1 || c.Lifecycle.MaxObservationAge < 1 {
		return fmt.Errorf("unsafe lifecycle policy")
	}
	if environment != "development" && (c.Lifecycle.QuarantineHours < 168 || c.Lifecycle.MinScanSpacing < 300) {
		return fmt.Errorf("stage/prod require at least seven-day quarantine and five-minute absence scan spacing")
	}
	if !c.Reconciliation.IncludeUntagged || !c.Reconciliation.IncludeAssociations || !c.Reconciliation.UnknownBlocksReuse || c.Reconciliation.FullScanInterval < 1 {
		return fmt.Errorf("reconciliation must include all occupancy and block UNKNOWN reuse")
	}
	domains := map[string]domain.Domain{}
	for _, d := range c.Domains {
		if !identifierPattern.MatchString(d.ID) || d.CoverageGeneration == "" || len(d.CloudCoverage) == 0 || !d.Backend.RequireUniquePrefixes || d.Backend.Type != "netbox" {
			return fmt.Errorf("invalid domain, coverage or inventory uniqueness")
		}
		if _, ok := domains[d.ID]; ok {
			return fmt.Errorf("duplicate domain %s", d.ID)
		}
		domains[d.ID] = d
		cells := map[string]bool{}
		for _, cell := range d.CloudCoverage {
			if !accountPattern.MatchString(cell.AccountID) || len(cell.Regions) == 0 || !strings.HasPrefix(cell.RoleARN, "arn:aws:iam::"+cell.AccountID+":role/") {
				return fmt.Errorf("invalid AWS coverage role or account")
			}
			for _, r := range cell.Regions {
				k := cell.AccountID + ":" + r
				if r == "" || cells[k] {
					return fmt.Errorf("empty or duplicate coverage cell")
				}
				cells[k] = true
			}
		}
	}
	ids := map[string]bool{}
	private := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")}
	for i, p := range c.Pools {
		d, ok := domains[p.DomainID]
		if !ok || ids[p.ID] || !identifierPattern.MatchString(p.ID) {
			return fmt.Errorf("invalid pool identity/domain")
		}
		ids[p.ID] = true
		prefix, err := netip.ParsePrefix(p.CIDR)
		if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
			return fmt.Errorf("pool CIDR must be canonical IPv4")
		}
		if !slices.ContainsFunc(private, func(parent netip.Prefix) bool {
			return parent.Bits() <= prefix.Bits() && parent.Contains(prefix.Addr())
		}) {
			return fmt.Errorf("v1 pools must be RFC1918")
		}
		if p.Scope != "vpc" || p.AddressFamily != "ipv4" || p.Environment == "" || p.Region == "" || len(p.AllowedPrefixLengths) == 0 || p.MaxAllocations < 1 || p.Backend.ParentPrefixID < 1 || len(p.EligibleTenants) == 0 || len(p.EligibleAccounts) == 0 {
			return fmt.Errorf("incomplete pool policy")
		}
		// ADR 0011: an empty (or whitespace-only) eligible_tenants entry would
		// store findings with an empty tenant (internal/service/worker.go's
		// fan-out takes the tenant straight from this list) and would make
		// eligibleString match a tenant-less principal in Pools/capacity.
		// Whitespace-only counts as empty -- there is no legitimate tenant ID
		// that is pure whitespace, and accepting one would let a padded string
		// slip past every tenant-equality comparison without ever matching a
		// real principal.
		for _, tenant := range p.EligibleTenants {
			if strings.TrimSpace(tenant) == "" {
				return fmt.Errorf("pool eligible_tenants entries must not be empty")
			}
		}
		for _, a := range p.EligibleAccounts {
			if !slices.ContainsFunc(d.CloudCoverage, func(cell domain.Cell) bool { return cell.AccountID == a && slices.Contains(cell.Regions, p.Region) }) {
				return fmt.Errorf("pool target lacks domain cloud coverage")
			}
		}
		for _, bits := range p.AllowedPrefixLengths {
			if bits < 16 || bits > 28 || bits < prefix.Bits() {
				return fmt.Errorf("invalid VPC allocation length")
			}
		}
		for _, raw := range p.ExcludedCIDRs {
			x, e := netip.ParsePrefix(raw)
			if e != nil || x != x.Masked() || x.Bits() < prefix.Bits() || !prefix.Contains(x.Addr()) {
				return fmt.Errorf("invalid pool exclusion")
			}
		}
		for _, other := range c.Pools[:i] {
			if other.DomainID == p.DomainID && prefix.Overlaps(netip.MustParsePrefix(other.CIDR)) {
				return fmt.Errorf("peer allocation pools overlap")
			}
		}
	}
	if c.SubnetPolicy.RequireParentScope != "vpc" || !c.SubnetPolicy.InheritParentTarget || c.SubnetPolicy.AllowNestedSubnets || c.SubnetPolicy.MaxChildren < 1 || len(c.SubnetPolicy.AllowedPrefixLengths) == 0 {
		return fmt.Errorf("invalid subnet policy")
	}
	for _, bits := range c.SubnetPolicy.AllowedPrefixLengths {
		if bits < 16 || bits > 28 {
			return fmt.Errorf("invalid subnet prefix length")
		}
	}
	subjects := map[string]bool{}
	for _, p := range c.Identities {
		// The duplicate-subject rule applies before the operator/tenant split
		// below (package G3b1): one subject can never be both.
		if p.Subject == "" || subjects[p.Subject] {
			return fmt.Errorf("identity mapping must be unique and explicitly scoped")
		}
		subjects[p.Subject] = true
		// A role that is neither empty nor "operator" fails the load rather
		// than quietly meaning "no role" -- a typo must not silently become a
		// privilege (or its absence). This reads p.Role only through the ""
		// comparison and through IsOperator(), never against OperatorRole or
		// the string "operator" directly; see the source-parsing test.
		if p.Role != "" && !p.IsOperator() {
			return fmt.Errorf("identity role must be empty or %q", domain.OperatorRole)
		}
		if p.IsOperator() {
			// ADR 0011 stage two: an operator is a principal with NO tenant.
			// Carrying tenant_id would make it a tenant with cross-tenant
			// reads and conflate the audit trail; carrying accounts,
			// environments or regions would sit on the principal looking
			// like constraints while constraining nothing, since only
			// validateRequest and capacity ever consult them and neither is
			// reachable without a tenant.
			if p.TenantID != "" || len(p.Accounts) > 0 || len(p.Environments) > 0 || len(p.Regions) > 0 {
				return fmt.Errorf("an operator identity must carry no tenant_id, accounts, environments or regions")
			}
			continue
		}
		if p.TenantID == "" || len(p.Accounts) == 0 || len(p.Environments) == 0 || len(p.Regions) == 0 {
			return fmt.Errorf("identity mapping must be unique and explicitly scoped")
		}
		for _, a := range p.Accounts {
			if !accountPattern.MatchString(a) {
				return fmt.Errorf("invalid identity account")
			}
		}
	}
	return nil
}

// IdentitiesWithoutPool returns the identities (in the order they are listed
// in cfg.Identities, which is the deterministic decode order of the identity
// file/section) whose tenant appears in no pool's eligible_tenants. An
// identity may legitimately be onboarded before its pool exists (ADR 0011),
// so this is a pure query for cmd/platform-ipam/main.go to log a warning
// from after config.Load, never a rule Validate enforces.
func IdentitiesWithoutPool(cfg domain.Config) []domain.Principal {
	eligible := map[string]bool{}
	for _, pool := range cfg.Pools {
		for _, tenant := range pool.EligibleTenants {
			eligible[tenant] = true
		}
	}
	out := []domain.Principal{}
	for _, id := range cfg.Identities {
		// An operator has no tenant by construction (Validate refuses
		// otherwise), so it is never "eligible" for any pool and never
		// "ineligible" either -- the warning this function feeds would be
		// false for it (package G3b1).
		if id.IsOperator() {
			continue
		}
		if !eligible[id.TenantID] {
			out = append(out, id)
		}
	}
	return out
}
