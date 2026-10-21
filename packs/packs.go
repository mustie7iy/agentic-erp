// Package packs embeds the released country packs (ADR-0048): one
// directory per country and version, packs/<cc>/<version>/, and the hash
// list released.sha256 that keeps released versions immutable.
// internal/countrypack loads, validates and applies them. The first packs
// (T62e, T62f) add their directories to the embed pattern.
package packs

import "embed"

// FS holds the packs and their hash list.
//
//go:embed released.sha256
var FS embed.FS
