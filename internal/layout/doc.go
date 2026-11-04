// Package layout holds the import-rule test of 06-engineering-process §6.7:
// which packages under internal/ may import which. It has no code of its
// own; a new dependency between contexts fails its test until it is added
// to the allowlist there, in the same change as the interface it uses and,
// if it turns a dependency around, an ADR (CLAUDE.md, Structure). It also
// holds TestNoSharedProblems, which refuses a package-level variable
// holding a *problem.Problem (review:T53f4).
package layout
