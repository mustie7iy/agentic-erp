# ADR-0011: Hash-chained audit events written in the same transaction

**Status:** proposed

## Context
Auditors need complete, tamper-evident evidence of who did what, including
agents and configuration changes.

## Decision
- `audit.audit_event` is insert-only, partitioned by month, with a hash chain
  per tenant (`prev_hash`, `hash` over a canonical encoding). Each business
  transaction writes its audit event in the same database transaction, so an
  action cannot succeed without its audit record.
- The chain head is periodically anchored to write-once object storage
  (S3 Object Lock) so even a database administrator cannot rewrite history
  unnoticed.
- Per-tenant chains avoid a global serialization point; chain extension takes
  a short row lock on the tenant's chain head.

## Alternatives
- **Audit via CDC/logs after the fact:** can be lost; not same-transaction.

## Consequences
- One extra insert per business transaction.
- Verification is a scan per tenant and period.

## Amendment (2026-10-21, T73c): where a request came from
The product owner decided that the API's audit trail records each
person's client address. Each event carries `client_address` (inet): a
person's address in the web workspace as erp-web saw it, otherwise the
client's connection as the router saw it through its load balancer; null
for work no request made (workers, workflows) and for events recorded
before. The router believes erp-web's forwarded address from erp-web's
SPIFFE identity alone, and the cell believes the router's from the
router's alone (ADR-0033, ADR-0052 "Built in T73c").

The address is personal data, so it sits beside the event, outside the
hash chain: the chain covers what happened, and the erasure of a tenant
(ADR-0021) blanks the addresses of its events without breaking the chain
or its anchors (ADR-0026). An event stays insert-only otherwise; the
erasure may change its address alone, to null. The address has the
event's retention. Only people holding `audit.event.read` see it on
`listAuditEvents` (product owner 2026-10-22: people only). Every other
reader, an agent, a service principal or any kind added later, gets each
event without it (the field omitted, not null), and the address is not
even read from the database for them. `reqctx.Masked` is an allow-list of
people, so a new kind of principal is masked until decided otherwise.

## As built (T101a, 2026-11-15): audit evidence exports

An auditor (permission `audit.evidence.export`, human-only) requests the
evidence of companies and a period with `POST /v1/audit-evidence-exports`
(`legal_entity_ids`, `from`, `to`). The request is queued in its
transaction as a row of `audit.evidence_export` (migration 000331) and
audited (`audit.evidence_export.request`); the evidence exporter
(`audit.EvidenceExporter`, a worker of every replica with object storage,
as the records archiver is, ADR-0079 §6) writes it in the background.
`GET /v1/audit-evidence-exports`, `GET /v1/audit-evidence-exports/{id}`
and `GET /v1/audit-evidence-exports/{id}/files/{name}` read the exports
and download their files.

1. **What is exported.** Four objects under
   `tenants/<tenant>/audit/evidence/<export id>/` (ADR-0016), format
   `agentic-erp.audit.evidence.v1`:
   - `audit_events.jsonl`: the tenant's audit events that occurred from
     `from` at 00:00 UTC to before the day after `to` in UTC, up to the
     chain's head when the exporter read it, by sequence number: `seq`,
     `occurred_at`, `actor_id`, `actor_kind`, `action`, `object_type`,
     `object_id`, `outcome`, `detail` as stored (it holds no personal
     data, ADR-0026), `trace_id`, and `client_address` (null for work no
     request made) only in an export made `with_client_addresses`;
     without hashes.
   - `journal_entries.jsonl`: the companies' journal entries posted from
     `from` to `to`, of every ledger, company by company in the order of
     their codes, then by posting date and id, each with its lines, in the
     ledger's record form (`ledger.EntryRecord`, the form of the records
     context's archive files, ADR-0079 §7, now shared).
   - `approval_decisions.jsonl`: the decisions on the approval requests
     whose object is one of those entries' source documents, whatever its
     kind (a manual entry's parked entry, an invoice, a payment run),
     request by request as raised and each request's decisions as taken:
     the request (id, object, title, status now), the entries it is about,
     the decision (id, step, `approve`, `reject` or `cancel`, comment,
     time) and who decided (`decided_by`, `decided_by_name` as the
     identity context gives it).
   - `manifest.json`: the export, the companies with their codes, the
     period and the audit events' instants, `with_client_addresses`, the
     requester, each file with its SHA-256, size and record count, and the
     chain's verification from its first event to its head against the
     stored hashes and the anchors, as `verifyAuditChain` checks them
     (`verified`, `last_seq`, `checked`, `anchors_checked`,
     `first_broken_seq`). A chain that does not verify still completes the
     export: where it broke is evidence too.
2. **The row carries the manifest's hash; the manifest carries the
   files'.** A download is checked against them (409
   `audit.evidence_changed`). A file is kept under its content's SHA-256
   (`audit_events.<sha256>.jsonl`), the manifest once under the export's
   id. A round run again after a commit whose acknowledgement was lost
   finds the export queued and the manifest kept: it writes any file it
   read differently from a period still open beside the first, and
   completes the export with the manifest kept, whose files are the
   first round's (run-twice test). One completion is audited
   (`audit.evidence_export.complete`) and published as
   AuditEvidenceExported on the platform's outbox, as AuditChainTampered
   is.
3. **Who sees what.** The export covers the companies asked for that the
   requester may act in, fixed when asked; when none, 422 names
   `legal_entity_ids` without saying which were unknown and which outside
   access. An export is listed, read and downloaded by a holder of
   `audit.evidence.export` who may act in every company it covers;
   otherwise it is not found. Client addresses are exported only when the
   requester held `audit.event.read` when asking, the permission that
   reads them on `listAuditEvents` (T73c: people only), and the audit
   events of such an export are handed only to a caller holding it too
   (403 `audit.client_addresses_withheld`); the manifest stays the
   export's to download.
4. **The run.** The exporter refuses a requester blocked or deprovisioned
   since (the export fails with `identity.principal_blocked`, PO
   2026-11-03), and fails an export whose kept manifest is not its own;
   the store or a port not answering leaves it queued. It acts as the
   system principal `urn:agentic-erp:audit-evidence-exporter`.
5. **Dependencies.** The audit context reads tenancy's legal entities,
   the ledger's journal (`ledger.Archive.EntriesBetween`), the approval
   requests (`approvals.Evidence`) and people's names through ports the
   composition root wires, by the types those contexts publish: a new
   arrow from audit to approvals, ledger and tenancy, none of which
   imports audit.
6. **Decided by default (for the product owner's digest):**
   - The audit events are the tenant's whole trail for the period, not a
     company's: an event names no company, and the chain verified is the
     tenant's. A holder of `audit.evidence.export` limited to some
     companies therefore receives the period's events of every company,
     as a holder of `audit.event.read` reads them today.
   - The period is a range of dates, not a fiscal period: the companies
     asked for may keep different calendars. Journal entries by posting
     date; audit events by UTC day. At most 371 days, the longest fiscal
     year (53 weeks).
   - Each request is a new export; unlike an archiving run, a second
     request does not answer the first, since the period may still be
     open.
   - The covered companies are fixed when the export is asked for; the
     exporter checks only that the requester is still active.
   - Files download as `application/octet-stream` whatever their name.

Later (plan-breadth-week3 §T101a): samples by the auditor's criteria,
evidence per control, subledger documents and attachments, a time-limited
auditor role, audit tools' formats, a signed manifest.
