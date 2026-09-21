package onboard

// Cases added in review of package C2: inputs real exporters produce that the
// first implementation rejected or mislabelled.

import (
	"strings"
	"testing"
)

func TestReadTextAcceptsBOMBeforeQuotedHeader(t *testing.T) {
	bom := string([]byte{0xEF, 0xBB, 0xBF})
	in := bom + "\"Account ID\",\"Region\",\"CIDR\"\r\n\"111111111111\",\"eu-central-1\",\"10.1.0.0/16\"\r\n"
	table, err := ReadText("export.csv", strings.NewReader(in), ReadOptions{})
	if err != nil {
		t.Fatalf("Excel-style BOM + quoted header rejected: %v", err)
	}
	if table.Kind != KindNetworks || len(table.Networks) != 1 {
		t.Fatalf("kind=%q rows=%d diags=%+v", table.Kind, len(table.Networks), table.Diagnostics)
	}
}

func TestReadTextSemicolonFileWithCommaInsideCell(t *testing.T) {
	in := "Account ID;Region;CIDR;Notes\n111111111111;eu-central-1;10.1.0.0/16;alt, bitte pruefen\n"
	table, err := ReadText("export.csv", strings.NewReader(in), ReadOptions{})
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if len(table.Networks) != 1 || table.Networks[0].CIDR != "10.1.0.0/16" {
		t.Fatalf("rows=%+v diags=%+v", table.Networks, table.Diagnostics)
	}
}

func TestReadTextReportsShortRowAtItsSpreadsheetRow(t *testing.T) {
	in := "Account ID,Region,CIDR\n111111111111,eu-central-1\n222222222222,eu-central-1,10.2.0.0/16\n"
	table, err := ReadText("x.csv", strings.NewReader(in), ReadOptions{})
	if err != nil {
		t.Fatalf("a short row must not abort the whole file: %v", err)
	}
	if len(table.Networks) != 1 {
		t.Errorf("rows=%d want 1", len(table.Networks))
	}
	found := false
	for _, d := range table.Diagnostics {
		if d.Row == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("short row 2 vanished without a diagnostic: %+v", table.Diagnostics)
	}
}
