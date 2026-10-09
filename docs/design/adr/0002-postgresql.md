# ADR-0002: PostgreSQL as the system of record

**Status:** proposed

## Context
ERP data is relational, constraint-heavy and must be strongly consistent for
postings. Volumes reach billions of journal lines per region.

## Decision
PostgreSQL 17: schema per bounded context, declarative partitioning for
high-volume tables, row-level security for tenant isolation, logical
replication/CDC for read models and analytics. Distribution beyond one
primary uses Citus (PostgreSQL extension) on `tenant_id` within a cell, and
more cells beyond that (ADR-0003).

## Alternatives
- **Distributed SQL (CockroachDB, Spanner, YugabyteDB):** horizontal writes
  built in, but higher latency per transaction, weaker ecosystem for
  partitioning/RLS patterns, vendor lock-in for Spanner.
- **Document store:** poor fit for constraints, joins within a context and
  double-entry invariants.

## Consequences
- Strong consistency and rich constraints (CHECK, EXCLUDE, deferred FKs).
- Write scale is bounded per primary; handled by partitioning, Citus and cells.
- Operating PostgreSQL at scale needs expertise; managed offerings are acceptable.
