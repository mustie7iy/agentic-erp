// Package packs embeds the released country packs (ADR-0048): one
// directory per country and version, packs/<cc>/<version>/, and the hash
// list released.sha256 that keeps released versions immutable.
// internal/countrypack loads, validates and applies them. Each country's
// directory is in the embed pattern: Canada (T62e).
package packs

import "embed"

// FS holds the packs and their hash list.
//
//go:embed released.sha256 ca
var FS embed.FS
