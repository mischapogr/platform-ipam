package onboardcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// testConfigYAML is a minimal, valid pools configuration (internal/config's
// Validate) with one domain "d1" (VRF 7) and one pool "pool1" covering
// 10.0.0.0/16, eligible for account 123456789012 in eu-central-1.
const testConfigYAML = `schema_version: 1
policy_version: "test-1"

overlap_domains:
  - id: d1
    description: test domain
    coverage_generation: "g1"
    inventory_backend:
      type: netbox
      vrf_id: 7
      require_unique_prefixes: true
    cloud_coverage:
      - account_id: "123456789012"
        role_arn: arn:aws:iam::123456789012:role/PlatformIpamReadOnly
        regions: [eu-central-1]

pools:
  - id: pool1
    overlap_domain: d1
    cidr: 10.0.0.0/16
    address_family: ipv4
    scope: vpc
    environment: prod
    region: eu-central-1
    eligible_tenants: [team-a]
    eligible_accounts: ["123456789012"]
    allowed_prefix_lengths: [20, 22]
    max_unreclaimed_allocations_per_tenant: 8
    inventory_backend:
      parent_prefix_id: 100

subnet_policy:
  require_parent_scope: vpc
  inherit_parent_target: true
  allowed_prefix_lengths: [24, 26]
  max_unreclaimed_children: 12
  allow_nested_subnets: false

lifecycle:
  reservation_expiry_enabled: false
  reservation_age_alert_hours: 24
  quarantine_hours: 168
  required_complete_absence_scans: 2
  minimum_scan_spacing_seconds: 300
  maximum_observation_age_seconds: 600

reconciliation:
  full_scan_interval_seconds: 300
  include_untagged_resources: true
  include_all_vpc_cidr_associations: true
  unknown_coverage_blocks_reuse: true

ui:
  inventory_links_enabled: false
`

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTableFile(t *testing.T, dir, name string, table onboard.Table) string {
	t.Helper()
	data, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, filepath.Join(dir, name), string(data))
}

// localServer binds an explicit IPv4 loopback listener, matching
// internal/netbox's own test helper, so the adapter's origin check never
// depends on how the host resolves "localhost".
func localServer(h http.Handler) *httptest.Server {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &httptest.Server{Listener: listener, Config: &http.Server{Handler: h}}
	s.Start()
	return s
}

func page(results []any) []byte {
	b, _ := json.Marshal(map[string]any{"count": len(results), "next": nil, "results": results})
	return b
}

// netboxStub is a minimal, stateful fake NetBox covering the read/write
// surface Snapshot and EnsureOccupancy need, in the httptest style of
// internal/netbox/*_test.go: GET list endpoints answer from in-memory state,
// POST appends to it and echoes the created object back. pluginInstalled
// additionally serves netbox-aws-vpc-plugin's accounts/vpcs/subnets
// endpoints and dcim regions, in the style of
// internal/netbox/awsplugin_test.go's awsPluginStack, for apply
// --aws-objects tests; when it is false every /api/plugins/ path 404s.
type netboxStub struct {
	vrfID           int
	prefixes        []map[string]any
	ranges          []map[string]any
	pluginInstalled bool
	awsAccounts     []map[string]any
	awsVPCs         []map[string]any
	awsSubnets      []map[string]any
	dcimRegions     []map[string]any
	tagIDs          map[string]int
	nextID          int
	requests        []string
	bodies          []map[string]any
}

// writes returns every non-GET request recorded so far, so a test can assert
// exactly how many writes an invocation made (and to which endpoint).
func (s *netboxStub) writes() []string {
	var out []string
	for _, r := range s.requests {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}
	return out
}

// Plugin and dcim paths this stub additionally serves when pluginInstalled
// is set, matching internal/netbox/awsplugin.go's own constants (this test
// package cannot import those unexported names, so the literal strings are
// duplicated here the same way internal/netbox/awsplugin_test.go duplicates
// them from the plugin's own real API).
const (
	awsAccountsPath = "/api/plugins/aws-vpc/aws-accounts/"
	awsVPCsPath     = "/api/plugins/aws-vpc/aws-vpcs/"
	awsSubnetsPath  = "/api/plugins/aws-vpc/aws-subnets/"
	dcimRegionsPath = "/api/dcim/regions/"
)

func (s *netboxStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	if s.nextID == 0 {
		s.nextID = 100
	}
	return localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)

		if !s.pluginInstalled && strings.HasPrefix(r.URL.Path, "/api/plugins/") {
			http.NotFound(w, r)
			return
		}

		switch {
		case r.URL.Path == awsAccountsPath:
			s.awsCollection(w, r, &s.awsAccounts, "account_id")
			return
		case r.URL.Path == awsVPCsPath:
			s.awsCollection(w, r, &s.awsVPCs, "vpc_id")
			return
		case r.URL.Path == awsSubnetsPath:
			s.awsCollection(w, r, &s.awsSubnets, "subnet_id")
			return
		case r.URL.Path == dcimRegionsPath:
			s.awsCollection(w, r, &s.dcimRegions, "slug")
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsAccountsPath):
			s.awsPatch(w, r, &s.awsAccounts, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsAccountsPath), "/"))
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsVPCsPath):
			s.awsPatch(w, r, &s.awsVPCs, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsVPCsPath), "/"))
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsSubnetsPath):
			s.awsPatch(w, r, &s.awsSubnets, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsSubnetsPath), "/"))
			return
		}

		// Detail read/write for one prefix -- package M9b3's RefreshOccupancy
		// needs both: a GET with an ETag (adoptRead, shared with Adopt and
		// AbandonAdoption) and a conditional PATCH merging custom_fields key
		// by key, the same way internal/netbox/adopt_test.go's adoptStack
		// already fakes NetBox 4.6.7's own behaviour (measured for package
		// F1, ADR 0010). This stub's ETag is constant per id and never
		// rejects an If-Match: the conflict/412 path already has thorough
		// coverage in internal/netbox/refresh_test.go, and this stub exists
		// to prove wiring, not to re-prove NetBox's own conditional semantics.
		if id, ok := detailPrefixID(r.URL.Path); ok {
			idx := s.findPrefix(id)
			switch r.Method {
			case http.MethodGet:
				if idx < 0 {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("ETag", `W/"`+strconv.Itoa(id)+`"`)
				_ = json.NewEncoder(w).Encode(s.prefixes[idx])
				return
			case http.MethodPatch:
				if idx < 0 {
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				s.bodies = append(s.bodies, body)
				s.applyPrefixPatch(idx, body)
				_ = json.NewEncoder(w).Encode(s.prefixes[idx])
				return
			}
		}

		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.nextID++
			id := s.nextID
			switch r.URL.Path {
			case "/api/ipam/prefixes/":
				// "tags" is echoed back, unlike before package M9b3: without
				// it every created prefix's Tags field decoded empty on a
				// later detail read, so RefreshOccupancy's own imported()
				// guard (defence in depth beyond ADR 0016's named refusals,
				// internal/netbox/refresh.go) refused every prefix this stub
				// itself had just created as an import.
				obj := map[string]any{"id": id, "prefix": body["prefix"], "vrf": map[string]any{"id": s.vrfID},
					"status": "active", "custom_fields": body["custom_fields"], "tags": body["tags"]}
				s.prefixes = append(s.prefixes, obj)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(obj)
			case "/api/ipam/ip-ranges/":
				obj := map[string]any{"id": id, "start_address": body["start_address"], "end_address": body["end_address"],
					"vrf": map[string]any{"id": s.vrfID}, "custom_fields": body["custom_fields"]}
				s.ranges = append(s.ranges, obj)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(obj)
			default:
				http.NotFound(w, r)
			}
			return
		}
		switch r.URL.Path {
		case "/api/ipam/vrfs/":
			w.Write(page([]any{map[string]any{"id": s.vrfID}}))
		case "/api/ipam/prefixes/":
			w.Write(page(toAny(s.prefixes)))
		case "/api/ipam/ip-addresses/":
			w.Write(page(nil))
		case "/api/ipam/ip-ranges/":
			w.Write(page(toAny(s.ranges)))
		default:
			http.NotFound(w, r)
		}
	}))
}

func toAny(m []map[string]any) []any {
	out := make([]any, len(m))
	for i, v := range m {
		out[i] = v
	}
	return out
}

func toIntValue(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func awsIDOf(m map[string]any) int {
	n, _ := toIntValue(m["id"])
	return n
}

// normalizeAWSObject mirrors real NetBox's write/read asymmetry for a
// nested-serializer field: a bare integer PK is accepted on write but always
// rendered back as a nested {"id": n} object on read (see
// internal/netbox/awsplugin.go's package comment and
// internal/netbox/awsplugin_test.go's identical fake behaviour).
func (s *netboxStub) normalizeAWSObject(obj map[string]any) {
	for _, key := range []string{"owner_account", "region", "vpc_cidr", "subnet_cidr", "vpc"} {
		if v, ok := obj[key]; ok && v != nil {
			if n, ok := toIntValue(v); ok {
				obj[key] = map[string]any{"id": n}
			}
		}
	}
	if v, ok := obj["tags"]; ok {
		if list, ok := v.([]any); ok {
			out := make([]any, 0, len(list))
			for _, item := range list {
				m, _ := item.(map[string]any)
				slug, _ := m["slug"].(string)
				if slug == "" {
					continue
				}
				if s.tagIDs == nil {
					s.tagIDs = map[string]int{}
				}
				id, ok := s.tagIDs[slug]
				if !ok {
					s.nextID++
					id = s.nextID
					s.tagIDs[slug] = id
				}
				out = append(out, map[string]any{"id": id, "slug": slug})
			}
			obj["tags"] = out
		}
	}
}

func (s *netboxStub) awsCollection(w http.ResponseWriter, r *http.Request, items *[]map[string]any, keyField string) {
	if r.Method == http.MethodPost {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.bodies = append(s.bodies, body)
		s.nextID++
		obj := map[string]any{"id": s.nextID}
		for k, v := range body {
			obj[k] = v
		}
		s.normalizeAWSObject(obj)
		*items = append(*items, obj)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
		return
	}
	q := r.URL.Query().Get(keyField)
	var out []any
	for _, it := range *items {
		if q == "" || it[keyField] == q {
			out = append(out, it)
		}
	}
	w.Write(page(out))
}

func (s *netboxStub) awsPatch(w http.ResponseWriter, r *http.Request, items *[]map[string]any, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.bodies = append(s.bodies, body)
	for i, it := range *items {
		if awsIDOf(it) != id {
			continue
		}
		for k, v := range body {
			(*items)[i][k] = v
		}
		s.normalizeAWSObject((*items)[i])
		b, _ := json.Marshal((*items)[i])
		w.Write(b)
		return
	}
	http.NotFound(w, r)
}

// poolPrefix is the pool's own, unmanaged prefix. Client.Snapshot requires
// exactly one matching prefix per configured pool, so every stub used by a
// plan/apply test must include it.
func poolPrefix(vrfID int) map[string]any {
	return map[string]any{"id": 1, "prefix": "10.0.0.0/16", "vrf": map[string]any{"id": vrfID}, "status": "active"}
}

// detailPrefixID recognizes a NetBox prefix detail path, mirroring
// internal/netbox/adopt_test.go's identically-named helper (a different
// package, so no name clash).
func detailPrefixID(path string) (int, bool) {
	const collection = "/api/ipam/prefixes/"
	if !strings.HasPrefix(path, collection) {
		return 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, collection), "/")
	if rest == "" {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	return id, err == nil
}

func (s *netboxStub) findPrefix(id int) int {
	for i, p := range s.prefixes {
		if n, ok := toIntValue(p["id"]); ok && n == id {
			return i
		}
	}
	return -1
}

// applyPrefixPatch mutates one stored prefix the way NetBox 4.6.7 does:
// custom_fields are merged key by key, every other top-level key replaces,
// and a key the body does not carry at all (like "tags", which
// RefreshOccupancy's payload never sends) is left untouched -- the same
// rule internal/netbox/adopt_test.go's adoptStack.apply already fakes.
func (s *netboxStub) applyPrefixPatch(idx int, body map[string]any) {
	x := s.prefixes[idx]
	for key, value := range body {
		if key == "custom_fields" {
			patch, _ := value.(map[string]any)
			fields, _ := x["custom_fields"].(map[string]any)
			if fields == nil {
				fields = map[string]any{}
			}
			for name, v := range patch {
				fields[name] = v
			}
			x["custom_fields"] = fields
			continue
		}
		x[key] = value
	}
}

// testEnv points IPAM_CONFIG_FILE/IPAM_NETBOX_URL/IPAM_NETBOX_TOKEN at a
// config file and a fake NetBox server for the duration of one test.
func testEnv(t *testing.T, netboxURL string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := writeFile(t, filepath.Join(dir, "pools.yaml"), testConfigYAML)
	t.Setenv("IPAM_CONFIG_FILE", cfgPath)
	t.Setenv("IPAM_IDENTITY_FILE", "")
	t.Setenv("IPAM_ENVIRONMENT", "development")
	t.Setenv("IPAM_NETBOX_URL", netboxURL)
	t.Setenv("IPAM_NETBOX_TOKEN", "test-token")
}

func networkRow(cidr, account, region, name, resourceID string, sourceRow int) onboard.NetworkRow {
	return onboard.NetworkRow{SourceRow: sourceRow, CIDR: cidr, AccountID: account, Region: region,
		Type: "vpc", ResourceID: resourceID, Name: name}
}

// --- parse ---

func TestParseCSVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, filepath.Join(dir, "networks.csv"),
		"cidr,account_id,region\n10.1.0.0/16,123456789012,eu-central-1\n")
	out := filepath.Join(dir, "table.json")
	var stdout, stderr bytes.Buffer

	code := Main(context.Background(), []string{"parse", in, "--out", out}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}

	table, err := loadTable(out)
	if err != nil {
		t.Fatalf("reading round-tripped table: %v", err)
	}
	if table.Kind != onboard.KindNetworks {
		t.Fatalf("Kind = %q, want %q", table.Kind, onboard.KindNetworks)
	}
	if len(table.Networks) != 1 {
		t.Fatalf("Networks = %+v, want exactly 1 row", table.Networks)
	}
	row := table.Networks[0]
	if row.CIDR != "10.1.0.0/16" || row.AccountID != "123456789012" || row.Region != "eu-central-1" {
		t.Fatalf("unexpected row: %+v", row)
	}
}

// TestParseStampsSourceFilePerInput is ADR 0014 / docs/WORK_PLAN.md M1b1's
// fix for row numbers becoming ambiguous once parse concatenates several
// inputs of the same kind: each input's NetworkRow.SourceRow independently
// restarts at 1 (internal/onboard's shiftRows numbers a row within its own
// file only), so without SourceFile a merged table.json could not say
// whether "row 1" belonged to a.csv or b.csv. It also covers the
// single-input case (TestParseCSVRoundTrip's shape): SourceFile is stamped
// there too, and every existing field stays exactly what it was.
func TestParseStampsSourceFilePerInput(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, filepath.Join(dir, "a.csv"),
		"cidr,account_id,region\n10.1.0.0/16,123456789012,eu-central-1\n")
	b := writeFile(t, filepath.Join(dir, "b.csv"),
		"cidr,account_id,region\n10.2.0.0/16,123456789012,eu-central-1\n")
	out := filepath.Join(dir, "table.json")
	var stdout, stderr bytes.Buffer

	code := Main(context.Background(), []string{"parse", a, b, "--out", out}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}

	table, err := loadTable(out)
	if err != nil {
		t.Fatalf("reading round-tripped table: %v", err)
	}
	if len(table.Networks) != 2 {
		t.Fatalf("Networks = %+v, want exactly 2 rows", table.Networks)
	}
	// Both rows have SourceRow 2 (the file's line 2: line 1 is the header,
	// which shiftRows counts): each input's own numbering restarts there
	// independently, and only SourceFile tells the two rows apart.
	for _, row := range table.Networks {
		if row.SourceRow != 2 {
			t.Errorf("row %+v: SourceRow = %d, want 2 (each input's row numbering restarts there independently)", row, row.SourceRow)
		}
	}
	fromA, fromB := 0, 0
	for _, row := range table.Networks {
		switch row.SourceFile {
		case a:
			fromA++
			if row.CIDR != "10.1.0.0/16" {
				t.Errorf("row from %s: CIDR = %q, want 10.1.0.0/16", a, row.CIDR)
			}
		case b:
			fromB++
			if row.CIDR != "10.2.0.0/16" {
				t.Errorf("row from %s: CIDR = %q, want 10.2.0.0/16", b, row.CIDR)
			}
		default:
			t.Errorf("row %+v: SourceFile = %q, want %q or %q", row, row.SourceFile, a, b)
		}
	}
	if fromA != 1 || fromB != 1 {
		t.Fatalf("fromA=%d fromB=%d, want exactly one row stamped from each input", fromA, fromB)
	}
}

func TestParseMixedKindsIsUsageError(t *testing.T) {
	dir := t.TempDir()
	networks := writeFile(t, filepath.Join(dir, "networks.csv"),
		"cidr,account_id,region\n10.1.0.0/16,123456789012,eu-central-1\n")
	accounts := writeFile(t, filepath.Join(dir, "accounts.csv"),
		"account_id\n123456789012\n")
	out := filepath.Join(dir, "table.json")
	var stdout, stderr bytes.Buffer

	code := Main(context.Background(), []string{"parse", networks, accounts, "--out", out}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage error); stderr=%s", code, ExitUsage, stderr.String())
	}
	msg := stderr.String()
	if !strings.Contains(msg, networks) || !strings.Contains(msg, accounts) {
		t.Fatalf("usage error does not name both inputs: %s", msg)
	}
	if !strings.Contains(msg, string(onboard.KindNetworks)) || !strings.Contains(msg, string(onboard.KindAccounts)) {
		t.Fatalf("usage error does not name both table kinds: %s", msg)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("%s was written despite the usage error", out)
	}
}

// --- plan ---

func TestPlanAgainstManagedPrefixReportsOverlapsManaged(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{
		poolPrefix(7),
		{"id": 2, "prefix": "10.0.1.0/24", "vrf": map[string]any{"id": 7}, "status": "active",
			"custom_fields": map[string]any{"platform_allocation_id": "alloc-1"}},
	}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.1.0/24", "123456789012", "eu-central-1", "collides", "vpc-0col", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan", tablePath, "--domain", "d1"}, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d (validation); stderr=%s", code, ExitValidation, stderr.String())
	}
	var report onboard.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not a Report: %v\n%s", err, stdout.String())
	}
	if !report.HasErrors() {
		t.Fatalf("report has no errors: %+v", report)
	}
	found := false
	for _, f := range report.Findings {
		if f.Rule == onboard.RuleOverlapsManaged {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected finding with rule %q, got %+v", onboard.RuleOverlapsManaged, report.Findings)
	}
}

func TestPlanCleanTableExitsZero(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "clean-vpc", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan", tablePath, "--domain", "d1"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}
	var report onboard.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not a Report: %v\n%s", err, stdout.String())
	}
	if report.HasErrors() {
		t.Fatalf("expected a clean report, got %+v", report.Findings)
	}
	if len(report.Writes) != 1 {
		t.Fatalf("Writes = %+v, want exactly 1", report.Writes)
	}
}

func TestPlanNetBoxUnreachableIsAdapterErrorNotAnEmptyInventory(t *testing.T) {
	// Start and immediately close a listener: connections to it are refused,
	// giving a deterministic "unreachable" NetBox without depending on a
	// real network timeout.
	server := localServer(http.NotFoundHandler())
	server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "x", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan", tablePath, "--domain", "d1"}, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want %d (adapter); stderr=%s", code, ExitAdapter, stderr.String())
	}
	// A Snapshot failure must never present as a report at all -- in
	// particular never as a "clean" (zero-error) one, which would look like
	// an empty, fully-occupied-nothing inventory rather than an unreadable one.
	if stdout.Len() != 0 {
		t.Fatalf("plan printed a report despite NetBox being unreachable: %s", stdout.String())
	}
}

// --- apply ---

func TestApplyRefusesWhenPlanHasErrorsAndWritesNothing(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	// This row is exactly the pool's own CIDR: RulePoolExactMatch, an error.
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.0.0/16", "123456789012", "eu-central-1", "bad", "vpc-0bad", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", tablePath, "--domain", "d1", "--batch", "batch-1"}, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d (validation); stderr=%s", code, ExitValidation, stderr.String())
	}
	if writes := stub.writes(); len(writes) != 0 {
		t.Fatalf("apply wrote %v despite plan reporting an error", writes)
	}
	if stdout.Len() != 0 {
		t.Fatalf("apply printed result lines despite refusing: %s", stdout.String())
	}
}

func TestApplyHappyPathThenSecondRunIsAllUnchanged(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "legacy-vpc", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)
	args := []string{"apply", tablePath, "--domain", "d1", "--batch", "batch-1", "--source", "networks.csv"}

	var stdout1, stderr1 bytes.Buffer
	code := Main(context.Background(), args, &stdout1, &stderr1)
	if code != ExitOK {
		t.Fatalf("first apply: exit = %d, want %d; stderr=%s", code, ExitOK, stderr1.String())
	}
	if writes := stub.writes(); len(writes) != 1 || writes[0] != "POST /api/ipam/prefixes/" {
		t.Fatalf("first apply writes = %v, want exactly one POST /api/ipam/prefixes/", writes)
	}
	var created applyResultLine
	if err := json.Unmarshal(bytes.TrimSpace(stdout1.Bytes()), &created); err != nil {
		t.Fatalf("first apply stdout is not a result line: %v\n%s", err, stdout1.String())
	}
	if created.CIDR != "10.0.5.0/24" || created.Action != "created" {
		t.Fatalf("unexpected first-run result: %+v", created)
	}

	before := len(stub.requests)
	var stdout2, stderr2 bytes.Buffer
	code = Main(context.Background(), args, &stdout2, &stderr2)
	if code != ExitOK {
		t.Fatalf("second apply: exit = %d, want %d; stderr=%s", code, ExitOK, stderr2.String())
	}
	var writesSinceFirst []string
	for _, r := range stub.requests[before:] {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			writesSinceFirst = append(writesSinceFirst, r)
		}
	}
	if len(writesSinceFirst) != 0 {
		t.Fatalf("second apply performed writes %v, want none (idempotent)", writesSinceFirst)
	}
	// onboard.Plan's RuleAlreadyUnmanaged (an info finding) drops a row that
	// is already present as unmanaged occupancy from the write set entirely
	// -- design section 5, "makes apply repeatable" -- so the second run's
	// replan yields zero WriteEntries for this row and apply calls
	// EnsureOccupancy zero times for it: no result line, not even an
	// "unchanged" one. That is a stronger idempotency guarantee (no NetBox
	// write-path call at all) than a repeated "unchanged" result would be.
	if stdout2.Len() != 0 {
		t.Fatalf("second apply printed %q, want no result lines (the row is now already-unmanaged, so plan drops it from the write set)", stdout2.String())
	}
}

// --- apply --refresh (package M9b3, ADR 0016) ---

func TestApplyWithoutRefreshFlagMakesNoAdditionalRequestsEvenWhenRefreshWouldWrite(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	first := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
	}}
	firstPath := writeTableFile(t, dir, "first.json", first)
	code := Main(context.Background(), []string{"apply", firstPath, "--domain", "d1", "--batch", "batch-1", "--source", "networks.csv"},
		&bytes.Buffer{}, &bytes.Buffer{})
	if code != ExitOK {
		t.Fatalf("first apply exit = %d", code)
	}

	// A second table naming a NEW VPC at the same CIDR: with --refresh this
	// would be exactly the RuleContributorNew/write case. Without the flag,
	// ADR 0016 requires apply to be byte-for-byte what it was before M9b3
	// existed -- not one additional request, GET included.
	second := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
		networkRow("10.0.5.0/24", "123456789099", "eu-central-1", "vpc-b", "vpc-0bbb", 2),
	}}
	secondPath := writeTableFile(t, dir, "second.json", second)
	before := len(stub.requests)
	var stdout bytes.Buffer
	code = Main(context.Background(), []string{"apply", secondPath, "--domain", "d1", "--batch", "batch-2", "--source", "networks.csv"},
		&stdout, &bytes.Buffer{})
	if code != ExitOK {
		t.Fatalf("second apply (no --refresh) exit = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("apply without --refresh printed %q, want nothing", stdout.String())
	}
	sinceFirst := stub.requests[before:]
	var nonGET []string
	for _, r := range sinceFirst {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			nonGET = append(nonGET, r)
		}
	}
	if len(nonGET) != 0 {
		t.Fatalf("apply without --refresh made write requests %v, want none", nonGET)
	}
	// Not even a detail read of the prefix: applyRefresh's whole loop is
	// gated behind the flag and RefreshOccupancy is never called, so no
	// GET to a prefix's own detail path (as opposed to the collection or
	// plugin list endpoints plan/apply always make) is issued either.
	for _, id := range []int{100, 101, 102} {
		want := http.MethodGet + " /api/ipam/prefixes/" + strconv.Itoa(id) + "/"
		for _, r := range sinceFirst {
			if r == want {
				t.Fatalf("apply without --refresh made a detail read %q it must never make", r)
			}
		}
	}
}

func TestApplyRefreshAddsANewContributorWithExactlyOnePATCH(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	first := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
	}}
	firstPath := writeTableFile(t, dir, "first.json", first)
	if code := Main(context.Background(), []string{"apply", firstPath, "--domain", "d1", "--batch", "batch-1", "--source", "networks.csv"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != ExitOK {
		t.Fatalf("first apply exit = %d", code)
	}

	second := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
		networkRow("10.0.5.0/24", "123456789099", "eu-central-1", "vpc-b", "vpc-0bbb", 2),
	}}
	secondPath := writeTableFile(t, dir, "second.json", second)
	before := len(stub.requests)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", secondPath, "--domain", "d1", "--batch", "batch-2", "--source", "networks.csv", "--refresh"},
		&stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("apply --refresh exit = %d; stderr=%s", code, stderr.String())
	}
	var patches []string
	for _, r := range stub.requests[before:] {
		if strings.HasPrefix(r, http.MethodPatch+" ") {
			patches = append(patches, r)
		}
	}
	if len(patches) != 1 {
		t.Fatalf("expected exactly one PATCH, got %v", patches)
	}

	lines := decodeJSONLines(t, stdout.Bytes())
	if len(lines) != 1 {
		t.Fatalf("expected exactly one result line, got %d: %v", len(lines), lines)
	}
	if lines[0]["action"] != "written" || lines[0]["refresh"] != true || lines[0]["cidr"] != "10.0.5.0/24" {
		t.Fatalf("unexpected refresh result line: %#v", lines[0])
	}
	if v, ok := lines[0]["reconstructed"]; ok && v == true {
		t.Fatalf("this prefix already carried a list; it must not be reported reconstructed: %#v", lines[0])
	}

	// Prefix ids are assigned starting at 100 by netboxStub; find the one
	// this table's CIDR actually landed on.
	prefixIdx := -1
	for i, p := range stub.prefixes {
		if p["prefix"] == "10.0.5.0/24" {
			prefixIdx = i
		}
	}
	if prefixIdx < 0 {
		t.Fatal("the imported prefix is missing from the stub")
	}
	fields, _ := stub.prefixes[prefixIdx]["custom_fields"].(map[string]any)
	contributors, _ := fields["platform_import_contributors"].([]any)
	if len(contributors) != 2 {
		t.Fatalf("expected two contributors after refresh, got %#v", contributors)
	}
}

func TestApplyRefreshSecondRunOverSameTableMakesNoPATCH(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
	}}
	path := writeTableFile(t, dir, "table.json", table)
	args := []string{"apply", path, "--domain", "d1", "--batch", "batch-1", "--source", "networks.csv", "--refresh"}
	if code := Main(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); code != ExitOK {
		t.Fatal("first apply --refresh failed")
	}

	// The prefix was just created (RuleAlreadyUnmanaged does not fire the
	// very first time), so run apply --refresh a SECOND time over the exact
	// same table: now it is already-unmanaged, the set does not change, and
	// no PATCH may happen.
	before := len(stub.requests)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("second apply --refresh exit = %d; stderr=%s", code, stderr.String())
	}
	var patches []string
	for _, r := range stub.requests[before:] {
		if strings.HasPrefix(r, http.MethodPatch+" ") {
			patches = append(patches, r)
		}
	}
	if len(patches) != 0 {
		t.Fatalf("an unchanged table must make no PATCH, got %v", patches)
	}
	if !strings.Contains(stderr.String(), "0 written") {
		t.Fatalf("stderr summary does not report 0 written: %s", stderr.String())
	}
}

func TestApplyRefreshReconstructsAPreExistingPrefixAndFlagsIt(t *testing.T) {
	// A prefix imported before ADR 0016 existed: tagged, unowned, and
	// carrying no platform_import_contributors key at all.
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{
		poolPrefix(7),
		{
			"id": 55, "prefix": "10.0.5.0/24", "vrf": map[string]any{"id": 7}, "status": "active",
			"tags":          []any{map[string]any{"slug": "platform-ipam-imported"}},
			"custom_fields": map[string]any{"platform_import_batch": "old-batch"},
		},
	}, nextID: 100}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "vpc-a", "vpc-0aaa", 1),
	}}
	path := writeTableFile(t, dir, "table.json", table)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"apply", path, "--domain", "d1", "--batch", "batch-1", "--source", "networks.csv", "--refresh"},
		&stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("apply --refresh exit = %d; stderr=%s", code, stderr.String())
	}
	lines := decodeJSONLines(t, stdout.Bytes())
	if len(lines) != 1 || lines[0]["reconstructed"] != true {
		t.Fatalf("expected one reconstructed result line, got %v", lines)
	}
	idx := stub.findPrefix(55)
	if idx < 0 {
		t.Fatal("prefix 55 vanished")
	}
	fields, _ := stub.prefixes[idx]["custom_fields"].(map[string]any)
	if fields["platform_import_contributors_reconstructed"] != true {
		t.Fatalf("the reconstructed flag was not stored: %#v", fields)
	}
	contributors, _ := fields["platform_import_contributors"].([]any)
	if len(contributors) != 1 {
		t.Fatalf("expected one reconstructed contributor, got %#v", contributors)
	}
	// The old batch is untouched: a refresh never rewrites
	// platform_import_batch (ADR 0016 names only the contributors field, the
	// source field, and the entries' own last_seen_batch).
	if fields["platform_import_batch"] != "old-batch" {
		t.Fatalf("platform_import_batch must not be touched by a refresh: %#v", fields["platform_import_batch"])
	}
}

func TestApplyRefreshIsANoOpForARangesTable(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	// A ranges table's own CIDR row (as opposed to a start/end row) goes
	// through the SAME planPrefixCandidates/RuleAlreadyUnmanaged path a
	// networks table's rows do (internal/onboard/plan.go), so this is the
	// one shape that could reach applyRefresh's loop on a second run if the
	// table.Kind guard were missing -- a start/end range never does, since
	// its idempotency is apply-time (matchOccupancyRange), not a Plan
	// finding, so it proves nothing about that guard.
	table := onboard.Table{Kind: onboard.KindRanges, Ranges: []onboard.RangeRow{
		{SourceRow: 1, CIDR: "10.0.9.0/24"},
	}}
	path := writeTableFile(t, dir, "table.json", table)
	if code := Main(context.Background(), []string{"apply", path, "--domain", "d1", "--batch", "batch-1", "--refresh"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != ExitOK {
		t.Fatal("first apply --refresh (ranges) failed")
	}

	// The second run's own plan must report the CIDR row already-unmanaged
	// -- otherwise this test would prove nothing about the table.Kind guard.
	planStdout, planStderr := &bytes.Buffer{}, &bytes.Buffer{}
	if code := Main(context.Background(), []string{"plan", path, "--domain", "d1"}, planStdout, planStderr); code != ExitOK {
		t.Fatalf("plan exit = %d; stderr=%s", code, planStderr.String())
	}
	if !strings.Contains(planStdout.String(), "already-unmanaged") {
		t.Fatalf("fixture sanity: the CIDR row must already be RuleAlreadyUnmanaged: %s", planStdout.String())
	}

	before := len(stub.requests)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", path, "--domain", "d1", "--batch", "batch-2", "--refresh"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("second apply --refresh (ranges) exit = %d; stderr=%s", code, stderr.String())
	}
	var patches []string
	for _, r := range stub.requests[before:] {
		if strings.HasPrefix(r, http.MethodPatch+" ") {
			patches = append(patches, r)
		}
	}
	if len(patches) != 0 {
		t.Fatalf("a ranges table must never trigger a refresh PATCH, got %v", patches)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a ranges table's --refresh pass printed %q, want nothing (it is already-unmanaged, so plan drops it from the write set, and table.Kind is not KindNetworks, so applyRefresh returns immediately)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--refresh: 0 written") {
		t.Fatalf("stderr summary does not report zero refresh activity: %s", stderr.String())
	}
}

// --- apply --aws-objects (package N3) ---

// decodeJSONLines splits raw JSON-Lines output into loosely-typed maps, so a
// test can inspect the "kind"/"action" fields of both applyResultLine
// (occupancy) and awsObjectResultLine (plugin objects) records without a
// shared type between them.
func decodeJSONLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out []map[string]any
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decoding JSON lines: %v\n%s", err, raw)
		}
		out = append(out, m)
	}
	return out
}

func countKinds(lines []map[string]any) map[string]int {
	counts := map[string]int{}
	for _, l := range lines {
		if k, ok := l["kind"].(string); ok {
			counts[k]++
		}
	}
	return counts
}

func pluginRequests(requests []string) []string {
	var out []string
	for _, r := range requests {
		if strings.Contains(r, "/api/plugins/") {
			out = append(out, r)
		}
	}
	return out
}

func TestApplyAWSObjectsFlagAbsentMakesNoPluginRequests(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}, pluginInstalled: true}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "legacy-vpc", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", tablePath, "--domain", "d1", "--batch", "batch-1"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}
	if reqs := pluginRequests(stub.requests); len(reqs) != 0 {
		t.Fatalf("apply without --aws-objects made plugin requests: %v", reqs)
	}
}

func TestApplyAWSObjectsPluginMissingExitsAdapterWithZeroOccupancyWrites(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}, pluginInstalled: false}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "legacy-vpc", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", tablePath, "--domain", "d1", "--batch", "batch-1", "--aws-objects"}, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want %d (adapter); stderr=%s", code, ExitAdapter, stderr.String())
	}
	if writes := stub.writes(); len(writes) != 0 {
		t.Fatalf("plugin-missing run wrote %v, want zero occupancy writes", writes)
	}
	if stdout.Len() != 0 {
		t.Fatalf("plugin-missing run printed %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "netbox-aws-vpc-plugin") {
		t.Fatalf("stderr does not explain the plugin is missing: %s", stderr.String())
	}
}

// TestApplyAWSObjectsHappyPathThenRepeatIsAllUnchanged is package N3's
// end-to-end scenario at the onboardcmd layer: a vpc row and a subnet row
// linked to it by parent_id both get plugin objects on the first run, and a
// second run -- where onboard.Plan's RuleAlreadyUnmanaged has dropped both
// rows' prefixes from the write set entirely -- still ensures (and finds
// unchanged) every plugin object, because applyAWSObjects walks
// table.Networks directly rather than report.Writes
// (docs/WORK_PLAN.md package N3: "including rows whose prefix already
// existed and was therefore dropped from the write set"). --aws-objects is
// placed before --domain in the args list here, a regression check for
// splitPositionalFlags: a boolean flag must never swallow the flag that
// follows it as its own value.
func TestApplyAWSObjectsHappyPathThenRepeatIsAllUnchanged(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}, pluginInstalled: true}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		{SourceRow: 1, CIDR: "10.0.5.0/24", AccountID: "123456789012", Region: "eu-central-1",
			Type: "vpc", ResourceID: "vpc-0abc", Name: "legacy-vpc"},
		{SourceRow: 2, CIDR: "10.0.5.0/26", AccountID: "123456789012", Region: "eu-central-1",
			Type: "subnet", ResourceID: "subnet-0xyz", ParentID: "vpc-0abc", Name: "legacy-subnet", AZID: "eu-central-1a"},
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)
	args := []string{"apply", "--aws-objects", tablePath, "--domain", "d1", "--batch", "batch-1"}

	var stdout1, stderr1 bytes.Buffer
	code := Main(context.Background(), args, &stdout1, &stderr1)
	if code != ExitOK {
		t.Fatalf("first apply: exit = %d, want %d; stderr=%s", code, ExitOK, stderr1.String())
	}
	lines := decodeJSONLines(t, stdout1.Bytes())
	kinds := countKinds(lines)
	if kinds["prefix"] != 2 || kinds["aws-account"] != 1 || kinds["aws-region"] != 1 || kinds["aws-vpc"] != 1 || kinds["aws-subnet"] != 1 {
		t.Fatalf("unexpected kind counts %v from lines %v", kinds, lines)
	}
	for _, l := range lines {
		if l["action"] != "created" {
			t.Fatalf("first apply line was not \"created\": %v", l)
		}
	}
	if !strings.Contains(stderr1.String(), "aws objects: 4 created, 0 updated, 0 unchanged, 4 total") {
		t.Fatalf("stderr missing the aws objects summary: %s", stderr1.String())
	}
	// AWSSubnet has no availability-zone field (internal/netbox/awsplugin.go's
	// package comment): the row's az_id must never reach a plugin request.
	for _, b := range stub.bodies {
		raw, _ := json.Marshal(b)
		if strings.Contains(string(raw), "eu-central-1a") {
			t.Fatalf("a request body carries the availability zone, which the plugin cannot store: %s", raw)
		}
	}

	before := len(stub.requests)
	var stdout2, stderr2 bytes.Buffer
	code = Main(context.Background(), args, &stdout2, &stderr2)
	if code != ExitOK {
		t.Fatalf("second apply: exit = %d, want %d; stderr=%s", code, ExitOK, stderr2.String())
	}
	var writesSinceFirst []string
	for _, r := range stub.requests[before:] {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			writesSinceFirst = append(writesSinceFirst, r)
		}
	}
	if len(writesSinceFirst) != 0 {
		t.Fatalf("second apply performed writes %v, want none (idempotent)", writesSinceFirst)
	}
	lines2 := decodeJSONLines(t, stdout2.Bytes())
	// Both rows' prefixes are now already-unmanaged and dropped from the
	// occupancy write set (no "prefix" lines), but their plugin objects are
	// still ensured -- and found unchanged -- because applyAWSObjects reads
	// table.Networks, not report.Writes.
	kinds2 := countKinds(lines2)
	if kinds2["prefix"] != 0 {
		t.Fatalf("second apply still wrote occupancy lines: %v", lines2)
	}
	if kinds2["aws-account"] != 1 || kinds2["aws-region"] != 1 || kinds2["aws-vpc"] != 1 || kinds2["aws-subnet"] != 1 {
		t.Fatalf("second apply did not re-ensure every plugin object: %v", kinds2)
	}
	for _, l := range lines2 {
		if l["action"] != "unchanged" {
			t.Fatalf("second apply line was not \"unchanged\": %v", l)
		}
	}
	if !strings.Contains(stderr2.String(), "aws objects: 0 created, 0 updated, 4 unchanged, 4 total") {
		t.Fatalf("stderr missing the aws objects summary: %s", stderr2.String())
	}
}

// --- render-config / render-fixture ---

func TestRenderConfigPrintsExpectedContent(t *testing.T) {
	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindAccounts, Accounts: []onboard.AccountRow{
		{SourceRow: 1, AccountID: "123456789012", TenantID: "team-a", Environment: "prod", Regions: []string{"eu-central-1"}},
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"render-config", tablePath}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"cloud_coverage:",
		`account_id: "123456789012"`,
		"eligible_accounts:",
		"eu-central-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render-config output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderFixturePrintsExpectedContent(t *testing.T) {
	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		networkRow("10.0.5.0/24", "123456789012", "eu-central-1", "legacy-vpc", "vpc-0abc", 1),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"render-fixture", tablePath, "--domain", "d1", "--generation", "g1"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if decoded["domain_id"] != "d1" || decoded["generation"] != "g1" || decoded["complete"] != true {
		t.Fatalf("unexpected fixture header: %+v", decoded)
	}
	resources, _ := decoded["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("resources = %+v, want exactly 1", decoded["resources"])
	}
}

// --- usage and dispatch ---

func TestMainWithNoArgsIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), nil, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestMainUnknownCommandIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"bogus"}, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestPlanRequiresDomainFlag(t *testing.T) {
	dir := t.TempDir()
	tablePath := writeTableFile(t, dir, "table.json", onboard.Table{Kind: onboard.KindNetworks})
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), []string{"plan", tablePath}, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("exit = %d, want %d (missing --domain)", code, ExitUsage)
	}
}

func TestApplyRequiresBatchFlag(t *testing.T) {
	dir := t.TempDir()
	tablePath := writeTableFile(t, dir, "table.json", onboard.Table{Kind: onboard.KindNetworks})
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", tablePath, "--domain", "d1"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (missing --batch)", code, ExitUsage)
	}
}
