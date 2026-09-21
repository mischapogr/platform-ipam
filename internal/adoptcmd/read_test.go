package adoptcmd

import (
	"strings"
	"testing"
)

const validHeader = "tenant_id,allocation_key,scope,environment,region,account_id,cidr,resource_id,netbox_prefix_id,parent_allocation_key,availability_zone_id\n"

func TestReadRecordsHappyPathCSV(t *testing.T) {
	csv := validHeader +
		"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n" +
		"team-a,subnet-orders-a,subnet,prod,eu-central-1,123456789012,10.1.0.0/26,subnet-0abc,43,vpc-orders,euc1-az1\n"
	records, err := ReadRecords("records.csv", strings.NewReader(csv))
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d: %#v", len(records), records)
	}
	if records[0].SourceRow != 2 || records[1].SourceRow != 3 {
		t.Fatalf("source rows: %d, %d", records[0].SourceRow, records[1].SourceRow)
	}
	if records[0].TenantID != "team-a" || records[0].AllocationKey != "vpc-orders" || records[0].Scope != "vpc" || records[0].CIDR != "10.1.0.0/24" {
		t.Fatalf("record 0: %#v", records[0])
	}
	if records[1].ParentAllocationKey != "vpc-orders" || records[1].AvailabilityZoneID != "euc1-az1" {
		t.Fatalf("record 1: %#v", records[1])
	}
}

func TestReadRecordsTSVByExtension(t *testing.T) {
	tsv := strings.ReplaceAll(validHeader, ",", "\t") +
		strings.ReplaceAll("team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n", ",", "\t")
	records, err := ReadRecords("records.tsv", strings.NewReader(tsv))
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(records) != 1 || records[0].AllocationKey != "vpc-orders" {
		t.Fatalf("records: %#v", records)
	}
}

func TestReadRecordsUnknownColumnIsAnError(t *testing.T) {
	csv := strings.TrimSuffix(validHeader, "\n") + ",prefix_length\n" +
		"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,,24\n"
	_, err := ReadRecords("records.csv", strings.NewReader(csv))
	if err == nil || !strings.Contains(err.Error(), "unknown column") || !strings.Contains(err.Error(), "prefix_length") {
		t.Fatalf("want an unknown-column error naming prefix_length, got %v", err)
	}
}

func TestReadRecordsMissingColumnIsAnError(t *testing.T) {
	header := strings.Replace(validHeader, "availability_zone_id", "", 1)
	header = strings.TrimSuffix(header, ",\n") + "\n"
	csv := header + "team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n"
	_, err := ReadRecords("records.csv", strings.NewReader(csv))
	if err == nil || !strings.Contains(err.Error(), "missing required column") || !strings.Contains(err.Error(), "availability_zone_id") {
		t.Fatalf("want a missing-column error naming availability_zone_id, got %v", err)
	}
}

func TestReadRecordsNoDataRowsIsAnError(t *testing.T) {
	_, err := ReadRecords("records.csv", strings.NewReader(validHeader))
	if err == nil || !strings.Contains(err.Error(), "no data rows") {
		t.Fatalf("want a no-data-rows error, got %v", err)
	}
}

func TestReadRecordsEmptyFileIsAnError(t *testing.T) {
	_, err := ReadRecords("records.csv", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no header row") {
		t.Fatalf("want a no-header-row error, got %v", err)
	}
}
