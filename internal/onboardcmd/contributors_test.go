package onboardcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// Package M9b1 (ADR 0016): the collapse by CIDR stops being lossy. Every row
// that resolved to one prefix becomes one contributor entry carrying ADR
// 0014's resource identity, so that a prefix shared by several VPCs finally
// says which ones -- instead of a sentence in a 200-rune description.

// contributorRow is the collector-shaped networks row these tests import: a
// file written AFTER package M1b1, so both new columns are present.
func contributorRow(cidr, account, resourceID string, sourceRow int) onboard.NetworkRow {
	return onboard.NetworkRow{
		SourceRow: sourceRow, CIDR: cidr, AccountID: account, Region: "eu-central-1",
		Type: "vpc", ResourceID: resourceID, Name: "vpc-" + account,
		AssociationID: "vpc-cidr-assoc-" + resourceID, ObservedAt: "2026-09-22T10:00:00Z",
		SourceFile:                 "networks.csv",
		AssociationIDColumnPresent: true, ObservedAtColumnPresent: true,
	}
}

func str(s string) *string { return &s }

// contributorCfg mirrors testConfigYAML's domain and pool as a value, so the
// pure Plan can be called without the environment a command needs.
func contributorCfg() domain.Config {
	return domain.Config{
		Domains: []domain.Domain{{ID: "d1", Backend: domain.Backend{Type: "netbox", VRFID: 7}}},
		Pools:   []domain.Pool{{ID: "pool1", DomainID: "d1", CIDR: "10.0.0.0/16"}},
	}
}

// occupancyFor runs the real entryOccupancy over a table and its plan, so the
// test exercises the collapse rule itself rather than a hand-built group.
func occupancyFor(t *testing.T, table onboard.Table, batch string) []netbox.Occupancy {
	t.Helper()
	report := onboard.Plan(table, contributorCfg(), "d1", domain.InventorySnapshot{Complete: true})
	if report.HasErrors() {
		t.Fatalf("the fixture table does not plan cleanly: %+v", report.Findings)
	}
	var out []netbox.Occupancy
	for _, entry := range report.Writes {
		occ, err := entryOccupancy(table, entry, batch, "networks.csv")
		if err != nil {
			t.Fatalf("entryOccupancy(%s): %v", entryLabel(entry), err)
		}
		out = append(out, occ)
	}
	return out
}

func TestTwoVPCsOfTwoAccountsAtOneCIDRBecomeTwoContributors(t *testing.T) {
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1),
		contributorRow("10.0.5.0/24", "000000000002", "vpc-0bbb", 2),
	}}
	occupancies := occupancyFor(t, table, "batch-1")
	if len(occupancies) != 1 {
		t.Fatalf("two rows at one CIDR produced %d writes, want one", len(occupancies))
	}

	want := []domain.Contributor{
		{
			Identity:  "aws:000000000001:eu-central-1:vpc-0aaa:10.0.5.0/24:vpc-cidr-assoc-vpc-0aaa",
			AccountID: "000000000001", Region: "eu-central-1", Type: "vpc",
			ResourceID: "vpc-0aaa", AssociationID: str("vpc-cidr-assoc-vpc-0aaa"),
			ObservedAt: str("2026-09-22T10:00:00Z"),
			// Both batches are this import's on a create; only a refresh
			// (package M9b3) can ever move them apart.
			FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
			SourceFile: "networks.csv", SourceRow: 1,
		},
		{
			Identity:  "aws:000000000002:eu-central-1:vpc-0bbb:10.0.5.0/24:vpc-cidr-assoc-vpc-0bbb",
			AccountID: "000000000002", Region: "eu-central-1", Type: "vpc",
			ResourceID: "vpc-0bbb", AssociationID: str("vpc-cidr-assoc-vpc-0bbb"),
			ObservedAt:     str("2026-09-22T10:00:00Z"),
			FirstSeenBatch: "batch-1", LastSeenBatch: "batch-1",
			SourceFile: "networks.csv", SourceRow: 2,
		},
	}
	if !reflect.DeepEqual(occupancies[0].Contributors, want) {
		t.Fatalf("contributors:\n got %s\nwant %s", pretty(t, occupancies[0].Contributors), pretty(t, want))
	}

	// The structured record is what the AWS custom fields could not carry:
	// networkAWSFields deliberately blanks them when two rows disagree about
	// the account, which is exactly the case this field exists for.
	if occupancies[0].AWSAccountID != "" || occupancies[0].AWSResourceID != "" {
		t.Fatalf("the ambiguous AWS fields are no longer blanked: %+v", occupancies[0])
	}
}

// The identity must be the same string internal/assess produces for the same
// record. ADR 0016: "the same string, not a similar one" -- a reviewer holding
// an `onboard assess` conflict and a prefix's contributor list joins them by
// this value, with no mapping table.
func TestContributorIdentityIsTheOneAssessProduces(t *testing.T) {
	row := contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1)
	got := networkContributors([]onboard.NetworkRow{row}, "batch-1")
	if len(got) != 1 {
		t.Fatalf("want one contributor, got %d", len(got))
	}
	if want := mapNetworkRow(row).Identity(); got[0].Identity != want {
		t.Fatalf("identity is %q, assess produces %q", got[0].Identity, want)
	}
}

// Package M9b2: Plan's contributor findings and apply's create path
// (entryOccupancy, via networkContributors) must never disagree about what a
// table's rows would write, or a re-import could raise a false
// contributor-new/-absent finding one run and write something different the
// next. Both now call the identical shared function,
// internal/onboard.ContributorsForRows -- this test calls it directly and
// compares against what apply's own path produces, so a second hand-written
// copy reappearing in either package (the exact drift package M9b1's review
// warned about) fails here.
func TestPlanAndApplyBuildIdenticalContributorsForTheSameRows(t *testing.T) {
	rows := []onboard.NetworkRow{
		contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1),
		contributorRow("10.0.5.0/24", "000000000002", "vpc-0bbb", 2),
	}
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: rows}
	occupancies := occupancyFor(t, table, "batch-1")
	if len(occupancies) != 1 {
		t.Fatalf("want one write, got %d", len(occupancies))
	}

	want := onboard.ContributorsForRows(rows, "batch-1")
	if !reflect.DeepEqual(occupancies[0].Contributors, want) {
		t.Fatalf("apply's contributors and the shared construction differ:\n apply: %s\n  want: %s",
			pretty(t, occupancies[0].Contributors), pretty(t, want))
	}
}

func TestSubnetContributorCarriesItsParentAndNoAssociation(t *testing.T) {
	row := contributorRow("10.0.6.0/24", "000000000003", "subnet-0abc", 1)
	row.Type = "subnet"
	row.ParentID = "vpc-0aaa"
	// The collector writes association_id for VPC rows only, so the column is
	// present and this cell is blank.
	row.AssociationID = ""

	got := networkContributors([]onboard.NetworkRow{row}, "batch-1")
	if len(got) != 1 {
		t.Fatalf("want one contributor, got %d", len(got))
	}
	if got[0].Type != "subnet" || got[0].ParentID != "vpc-0aaa" || got[0].ResourceID != "subnet-0abc" {
		t.Fatalf("subnet contributor is wrong: %+v", got[0])
	}
	if got[0].AssociationID != nil {
		t.Fatalf("a subnet has no CIDR association: %q", *got[0].AssociationID)
	}
	if got[0].ObservedAt == nil || *got[0].ObservedAt != "2026-09-22T10:00:00Z" {
		t.Fatalf("a subnet's observation time is still evidence: %+v", got[0].ObservedAt)
	}
	if !strings.HasPrefix(got[0].Identity, "aws:000000000003:eu-central-1:subnet-0abc:10.0.6.0/24") {
		t.Fatalf("unexpected subnet identity %q", got[0].Identity)
	}
}

// A file written before package M1b1 has neither column. ADR 0016: such a
// contributor's provenance is unknown, it carries null, and it can never take
// part in a removal. Nothing may be guessed to fill the gap.
func TestAnOldFormatFileProducesNullObservationAndAssociation(t *testing.T) {
	for name, row := range map[string]onboard.NetworkRow{
		"no such columns": {
			SourceRow: 1, CIDR: "10.0.7.0/24", AccountID: "000000000004", Region: "eu-central-1",
			Type: "vpc", ResourceID: "vpc-0old", SourceFile: "legacy.csv",
		},
		"columns present, cells blank": {
			SourceRow: 1, CIDR: "10.0.7.0/24", AccountID: "000000000004", Region: "eu-central-1",
			Type: "vpc", ResourceID: "vpc-0old", SourceFile: "legacy.csv",
			AssociationIDColumnPresent: true, ObservedAtColumnPresent: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := networkContributors([]onboard.NetworkRow{row}, "batch-1")
			if got[0].ObservedAt != nil {
				t.Fatalf("observed_at is %q, want null: a blank cell is no more an observation than a missing column", *got[0].ObservedAt)
			}
			if got[0].AssociationID != nil {
				t.Fatalf("association_id is %q, want null", *got[0].AssociationID)
			}
			// Without an association the identity is the four-part form, and
			// ADR 0014 already records the consequence: two associations of
			// one VPC at one CIDR cannot be told from one read twice.
			if want := "aws:000000000004:eu-central-1:vpc-0old:10.0.7.0/24"; got[0].Identity != want {
				t.Fatalf("identity is %q, want %q", got[0].Identity, want)
			}
		})
	}
}

// Determinism: the same table must produce the same list however the rows
// arrive, or a re-import that only reordered a file would look to package
// M9b2 like a changed contributor set and cause a write.
func TestContributorOrderIsDeterministicAcrossInputOrder(t *testing.T) {
	a := contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1)
	a.SourceFile = "a.csv"
	b := contributorRow("10.0.5.0/24", "000000000002", "vpc-0bbb", 1)
	b.SourceFile = "b.csv"
	c := contributorRow("10.0.5.0/24", "000000000003", "vpc-0ccc", 2)
	c.SourceFile = "a.csv"

	first := networkContributors([]onboard.NetworkRow{a, b, c}, "batch-1")
	second := networkContributors([]onboard.NetworkRow{c, b, a}, "batch-1")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reordering the rows changed the list:\n%s\n%s", pretty(t, first), pretty(t, second))
	}
	// Two files whose row numbers both restart at 1 must not be interleaved
	// by row number alone -- M1b1 added SourceFile for exactly that reason.
	wantOrder := []string{"a.csv/1", "a.csv/2", "b.csv/1"}
	var gotOrder []string
	for _, entry := range first {
		gotOrder = append(gotOrder, entry.SourceFile+"/"+strconv.Itoa(entry.SourceRow))
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("order is %v, want %v", gotOrder, wantOrder)
	}
}

// A single-row import is the common case and ADR 0016 is explicit that it
// changes nothing else: the AWS custom fields and the description stay exactly
// what they were before this package.
func TestASingleRowImportIsUnchangedApartFromTheNewField(t *testing.T) {
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1),
	}}
	occupancies := occupancyFor(t, table, "batch-1")
	if len(occupancies) != 1 {
		t.Fatalf("want one write, got %d", len(occupancies))
	}
	occ := occupancies[0]
	if occ.AWSAccountID != "000000000001" || occ.AWSRegion != "eu-central-1" || occ.AWSResourceID != "vpc-0aaa" {
		t.Fatalf("the AWS fields changed: %+v", occ)
	}
	if want := mergeNetworkDescription(table.Networks); occ.Description != want {
		t.Fatalf("description is %q, want %q", occ.Description, want)
	}
	if len(occ.Contributors) != 1 {
		t.Fatalf("want one contributor, got %d", len(occ.Contributors))
	}
}

// ADR 0016 defers the description deliberately ("a fifth, deliberately
// separated" package), so a CIDR shared by enough VPCs still overflows the
// 200-rune limit and still fails the import at EnsureOccupancy. Pinning it
// here keeps that a decision rather than something a later reader assumes
// M9b1 quietly fixed.
func TestTheDescriptionStillGrowsWithEverySharer(t *testing.T) {
	var rows []onboard.NetworkRow
	for i := 0; i < 12; i++ {
		row := contributorRow("10.0.5.0/24", "00000000000"+strconv.Itoa(i%10), "vpc-0000000"+string(rune('a'+i)), i+1)
		row.Name = "a-reasonably-long-vpc-name-" + row.ResourceID
		rows = append(rows, row)
	}
	occupancies := occupancyFor(t, onboard.Table{Kind: onboard.KindNetworks, Networks: rows}, "batch-1")
	if len(occupancies[0].Contributors) != len(rows) {
		t.Fatalf("every sharer must be recorded: %d of %d", len(occupancies[0].Contributors), len(rows))
	}
	if got := len([]rune(occupancies[0].Description)); got <= 200 {
		t.Fatalf("this fixture no longer overflows the 200-rune description (%d runes); "+
			"if the deferred description package has landed, this test is what should change", got)
	}
}

func TestRangeEntriesCarryNoContributors(t *testing.T) {
	table := onboard.Table{Kind: onboard.KindRanges, Ranges: []onboard.RangeRow{
		{SourceRow: 1, StartAddress: "10.0.8.10", EndAddress: "10.0.8.20", Owner: "netops"},
		{SourceRow: 2, CIDR: "10.0.9.0/24", Owner: "netops"},
	}}
	entries := occupancyFor(t, table, "batch-1")
	if len(entries) != 2 {
		t.Fatalf("want two range entries, got %d", len(entries))
	}
	for _, occ := range entries {
		if occ.Contributors != nil {
			t.Fatalf("a ranges-table entry carried contributors: %+v", occ.Contributors)
		}
	}
}

// End of the command's own path: the create request a real apply sends really
// carries the list, so the wiring between entryOccupancy and the adapter is
// covered and not just each half.
func TestApplyWritesTheContributorListToNetBox(t *testing.T) {
	stub := &netboxStub{vrfID: 7, prefixes: []map[string]any{poolPrefix(7)}}
	server := stub.server(t)
	defer server.Close()
	testEnv(t, server.URL)

	dir := t.TempDir()
	table := onboard.Table{Kind: onboard.KindNetworks, Networks: []onboard.NetworkRow{
		contributorRow("10.0.5.0/24", "000000000001", "vpc-0aaa", 1),
		contributorRow("10.0.5.0/24", "000000000002", "vpc-0bbb", 2),
	}}
	tablePath := writeTableFile(t, dir, "table.json", table)

	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", tablePath, "--domain", "d1",
		"--batch", "batch-1", "--source", "networks.csv"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("apply exit = %d, want %d; stderr=%s", code, ExitOK, stderr.String())
	}
	if writes := stub.writes(); len(writes) != 1 || writes[0] != "POST /api/ipam/prefixes/" {
		t.Fatalf("writes = %v, want exactly one prefix create", writes)
	}

	// The stub stores what the create asked for (its POST branch copies the
	// body's custom_fields onto the object it keeps), so the stored prefix is
	// what the adapter really sent.
	var created map[string]any
	for _, x := range stub.prefixes {
		if x["prefix"] == "10.0.5.0/24" {
			created = x
		}
	}
	if created == nil {
		t.Fatalf("the imported prefix was not created: %v", stub.prefixes)
	}
	fields, _ := created["custom_fields"].(map[string]any)
	raw, err := json.Marshal(fields[netbox.ImportContributorsField])
	if err != nil {
		t.Fatal(err)
	}
	var sent []domain.Contributor
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("the create did not carry a contributor array: %s", raw)
	}
	if len(sent) != 2 {
		t.Fatalf("the create carried %d contributors, want two: %s", len(sent), raw)
	}
	if sent[0].ResourceID != "vpc-0aaa" || sent[1].ResourceID != "vpc-0bbb" {
		t.Fatalf("unexpected contributors: %s", raw)
	}
	if sent[0].SourceRow != 1 || sent[1].SourceRow != 2 {
		t.Fatalf("each contributor must name its own source row: %s", raw)
	}
}

func pretty(t *testing.T, v any) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
