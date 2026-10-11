# Migration strategy

## Objective

Migrate off SAP without disruption, at a fraction of the cost of a
conventional S/4HANA migration or ERP replacement.

## Where the cost of a conventional program comes from

| Cost driver | Conventional approach | This approach |
|---|---|---|
| Big-bang cutover risk | Everything must work on day one, so testing and contingency costs grow | One scope at a time. Each cutover is small and can be reversed (SAP keeps running until the scope is proven) |
| Re-implementing SAP screens and transactions | Thousands of transactions rebuilt or retrained | Users work through review queues and agents; most SAP screens are never rebuilt |
| Consultants for mapping and testing | Manual mapping specs and test cycles | Mapping in code, automated data-quality gate, automated reconciliation on every rehearsal |
| SAP licences during transition | Full licence until the final cutover | Licensed scope shrinks as each company code moves (subject to the SAP contract) |
| Upgrade treadmill | ECC → S/4HANA migration program | Avoided. SAP only has to stay readable until each scope is migrated |

The size of the saving depends on the client's SAP contract, the number of
scopes, and how much custom ABAP has to be replaced. Measure it per scope
rather than promising one number up front.

## Cutover protocol per scope (company code + fiscal year)

1. **Profile.** Run `erp migrate -rehearsal` (a migration run that loads,
   reconciles and rolls back, keeping only its report). Fix data-quality *errors* in
   SAP. Get business sign-off on each *warning* (duplicate vendors, missing tax
   numbers, blocked vendors with open items).
2. **Rehearse.** Repeat full migrations until reconciliation passes with no
   differences, at least three times in a row.
3. **Parallel run.** Process the scope in both systems for one period-end.
   Compare the outputs (open items, trial balance, invoices posted).
4. **Cut over.** Freeze postings in SAP for the scope, run the final delta
   migration, reconcile, and sign off. Switch users to the new flow.
5. **Read-only SAP.** Keep SAP readable for audit until the retention period
   allows archiving.

A scope may only be cut over when `erp migrate` exits 0: no load rejections
and every reconciliation check passing.

## Choosing a mode

| Mode | Loads | Use for |
|---|---|---|
| `documents` | Every Universal Journal document, with SAP numbers kept in the IDs | The current fiscal year, when auditors want document-level history in the new system |
| `balances` | One opening entry per company, with open vendor items kept as individual lines | Closed years, or when history stays in archived SAP |

## Reconciliation controls

Each control is computed twice: from the raw SAP extract and from the
platform, read through the owning contexts.
They must be equal.

- Trial balance per account (differences listed by account)
- Total debits posted (`documents` mode)
- Open payables, in total and per vendor
- Vendor count per company code
- Purchase order count and value
- Parked supplier invoices (posted ones are carried by their FI documents)
- Every SAP document in scope present on the platform (missing ones listed)

## What to check on a real SAP system

- OData services are activated (`/IWFND/MAINT_SERVICE`) and the technical user
  has read authorization only.
- Entity and field names in `internal/sap/odata.go` match your release (check `$metadata`).
- Amounts for currencies with no decimals or three decimals (JPY, KWD) come in
  external format.
- **SAP digital-access licensing:** reading data is usually fine, but agents
  creating documents in SAP may count as indirect use. This design never
  writes to SAP. Confirm with the contract owner anyway.
