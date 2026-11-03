// Package layout holds the import-rule test of 06-engineering-process §6.7:
// which packages under internal/ may import which. It has no code of its
// own; a new dependency between contexts fails its test until it is added
// to the allowlist there, in the same change as the interface it uses and,
// if it turns a dependency around, an ADR (CLAUDE.md, Structure). It also
// holds the rules that, like the import rules, span every package and so
// must run in every lane: TestPageScopes, that a page token's scope is
// built from values and not pointers (04 §4.5).
package layout
