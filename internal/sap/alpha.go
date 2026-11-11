package sap

import "strings"

// Alpha strips SAP ALPHA-conversion leading zeros ("0000100001" ->
// "100001"); an all-zero number is "0" and "" stays "". The configuration
// draft (delivery/sapconfig) numbers chart accounts with it and the
// migration (integration) looks accounts up in that chart with it, so both
// must use this one function.
func Alpha(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" && s != "" {
		return "0"
	}
	return t
}

// CompanyID is a company ID (T001-RCOMP, LFA1-VBUND, ACDOCA-RASSC) in the
// form the three are compared in: without blanks and ALPHA zeros, "" for
// none. That SAP holds them in one ALPHA form is stated from memory
// (docs/migration.md, "What to check on a real SAP system"); FilterScope
// keys the company codes by it and the migration looks a vendor's trading
// partner up with it, so both must use this one function.
func CompanyID(raw string) string { return Alpha(strings.TrimSpace(raw)) }
