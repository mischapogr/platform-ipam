package onboard

import (
	"fmt"
	"strings"
	"unicode"
)

// Canonical column names. These are the values ResolveHeaders puts into its
// returned map, and the field names row builders in normalize.go read.
const (
	colAccountID     = "account_id"
	colAccountName   = "account_name"
	colEnvironment   = "environment"
	colTenantID      = "tenant_id"
	colRegions       = "regions"
	colRoleARN       = "role_arn"
	colOwner         = "owner"
	colDescription   = "description"
	colCIDR          = "cidr"
	colRegion        = "region"
	colType          = "type"
	colResourceID    = "resource_id"
	colParentID      = "parent_id"
	colAZID          = "az_id"
	colName          = "name"
	colState         = "state"
	colPrimary       = "primary"
	colStartAddress  = "start_address"
	colEndAddress    = "end_address"
	colSource        = "source"
	colAssociationID = "association_id"
	colObservedAt    = "observed_at"
)

// headerAliases maps a normalizeHeaderKey'd header to its canonical column.
// Keys are case-insensitive and punctuation/whitespace-insensitive by
// construction (see normalizeHeaderKey), so "Account-Nr.", "AccountId" and
// "Account ID" all land on the same key.
var headerAliases = map[string]string{
	"accountid":            colAccountID,
	"accountnr":            colAccountID,
	"awskonto":             colAccountID,
	"awsaccount":           colAccountID,
	"accountname":          colAccountName,
	"cidr":                 colCIDR,
	"cidrblock":            colCIDR,
	"iprange":              colCIDR,
	"netz":                 colCIDR,
	"network":              colCIDR,
	"prefix":               colCIDR,
	"region":               colRegion,
	"regions":              colRegions,
	"vpcid":                colResourceID,
	"subnetid":             colResourceID,
	"resourceid":           colResourceID,
	"az":                   colAZID,
	"azid":                 colAZID,
	"availabilityzoneid":   colAZID,
	"env":                  colEnvironment,
	"environment":          colEnvironment,
	"tenant":               colTenantID,
	"tenantid":             colTenantID,
	"owner":                colOwner,
	"description":          colDescription,
	"notes":                colDescription,
	"start":                colStartAddress,
	"startaddress":         colStartAddress,
	"end":                  colEndAddress,
	"endaddress":           colEndAddress,
	"rolearn":              colRoleARN,
	"type":                 colType,
	"name":                 colName,
	"state":                colState,
	"primary":              colPrimary,
	"parentid":             colParentID,
	"source":               colSource,
	"associationid":        colAssociationID,
	"cidrassociationid":    colAssociationID,
	"vpccidrassociationid": colAssociationID,
	"observedat":           colObservedAt,
	"observed":             colObservedAt,
	"observationtime":      colObservedAt,
	"observeddate":         colObservedAt,
}

// knownColumns lists, per TableKind, the canonical columns a row builder
// reads directly. A resolved column outside this set for its kind is kept
// (ResolveHeaders never drops it) but ends up appended to the row's
// Description instead of a dedicated field.
var knownColumns = map[TableKind]map[string]bool{
	KindAccounts: {
		colAccountID: true, colAccountName: true, colEnvironment: true,
		colTenantID: true, colRegions: true, colRoleARN: true,
		colOwner: true, colDescription: true,
	},
	KindNetworks: {
		colCIDR: true, colAccountID: true, colRegion: true, colType: true,
		colResourceID: true, colParentID: true, colAZID: true, colName: true,
		colEnvironment: true, colState: true, colPrimary: true, colDescription: true,
		colAssociationID: true, colObservedAt: true,
	},
	KindRanges: {
		colCIDR: true, colStartAddress: true, colEndAddress: true,
		colDescription: true, colOwner: true, colSource: true,
	},
}

// normalizeHeaderKey lowercases and strips everything but letters and
// digits, making header matching case- and punctuation/whitespace-insensitive
// as design section 3 requires.
func normalizeHeaderKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ResolveHeaders matches a header row against the alias table and decides
// the table kind from the required-columns rule in design section 3: all of
// cidr+account_id+region resolved means networks; cidr, or both
// start_address and end_address, means ranges; account_id alone means
// accounts. Networks is checked before ranges before accounts, so a header
// row satisfying a more specific kind is never misread as a looser one.
//
// columns maps header index to its canonical column name for a recognized
// alias, or to the original (trimmed) header text when no alias matches —
// so an unknown column is never dropped, only left for the caller to treat
// as an unknown column.
func ResolveHeaders(headers []string) (TableKind, map[int]string, error) {
	columns := make(map[int]string, len(headers))
	for i, h := range headers {
		clean := cleanCell(h)
		if clean == "" {
			continue
		}
		if canon, ok := headerAliases[normalizeHeaderKey(clean)]; ok {
			columns[i] = canon
		} else {
			columns[i] = clean
		}
	}

	resolved := make(map[string]bool, len(columns))
	for _, c := range columns {
		resolved[c] = true
	}

	switch {
	case resolved[colCIDR] && resolved[colAccountID] && resolved[colRegion]:
		return KindNetworks, columns, nil
	case resolved[colCIDR] && resolved[colAccountID]:
		// A CIDR with an account but no region is a networks table with a
		// column missing, not a ranges table. Falling through would import it
		// as anonymous ranges and demote the account to description text --
		// a silent reinterpretation of the operator's data.
		return "", nil, fmt.Errorf(
			"table has cidr and account_id but no region column, so it cannot be read as networks; "+
				"add a region column, or remove account_id to import plain ranges (headers seen: %s)",
			strings.Join(headers, ", "))
	case resolved[colCIDR] || (resolved[colStartAddress] && resolved[colEndAddress]):
		return KindRanges, columns, nil
	case resolved[colAccountID]:
		return KindAccounts, columns, nil
	}

	seen := make([]string, 0, len(headers))
	for _, h := range headers {
		if c := cleanCell(h); c != "" {
			seen = append(seen, c)
		}
	}
	return "", nil, fmt.Errorf(
		"could not resolve a required column for accounts (account_id), networks (cidr, account_id, region), "+
			"or ranges (cidr, or start_address and end_address); headers seen: %s",
		strings.Join(seen, ", "))
}
