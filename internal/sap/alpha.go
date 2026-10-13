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
