# Depth plan 3: third variant slices (18 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants go on, pilot-relevant first). It follows `plan-depth-1.md`
and `plan-depth-2.md`, whose rules it does not repeat: each lane splits
two named variants of its placeholder into slices in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later) and builds them.
`docs/tracker.json` stays the only plan (07 §7.5); each leaf's `r` names
its section below. Each lane writes its own `## <lane>` section.

## alloc

Placeholder: "later: allocation cycle variants" (`plan-breadth-week1.md`
§T60g, Later; ADR-0064 §5). Two variants are split here: shares from
statistical key figures, and fixed amounts with the reversal of a run.
SAP S/4HANA's assessment and distribution cycles (KSU1/KSV1 with their
segments' sender and receiver rules, statistical key figures KK01 with
their values posted by KB31N, cycle reversal from KSU5/KSV5) are the
reference, stated from memory and marked so in ADR-0064's "As built".
A cycle stays one segment (one sender, its receivers): several segments
and senders in a cycle stay in the placeholder.

### T60j Receiver shares from statistical key figures

**Capability.** A controller defines a statistical key figure of a
company (employees, floor space), posts its values per cost center and
period, and defines a cycle whose receivers' shares are the key figure's
values of the period run: a run splits the sender's costs in proportion
to them.

- **Reuses:** T60f/T60g's cycles, their run, lock and entry
  (`money.Allocate` over exact fractions); the ledger's port for the
  centers (`CostCenterRefs`); the company's regular periods
  (`regularPeriod`); `ref.unit_of_measure`; audit; the controlling
  outbox.
- **Adds:** tables `controlling.statistical_key_figure` (code unique per
  company, name, unit of measure; not changed in the first version) and
  `controlling.statistical_key_figure_value` (insert-only: one row per
  cost center of a posting, a non-zero quantity with up to six
  decimals; a center's value of a period is the sum of its rows);
  `POST /v1/statistical-key-figures`, `GET` (list, by code) and
  `GET /v1/statistical-key-figures/{id}`; `POST
  /v1/statistical-key-figures/{id}/postings` (a period's values for
  cost centers of the company); `GET
  /v1/statistical-key-figures/{id}/values` (a period's value per cost
  center with the total); permissions `controlling.key_figure.read` and
  `.write`; the cycle's `share_basis` (`fixed_percentages`, the default,
  or `statistical_key_figure` naming the key figure, whose receivers
  carry no percentage); events `StatisticalKeyFigureChanged`,
  `StatisticalKeyFigurePosted`, and `AllocationCycleChanged` .v2 beside
  .v1 (whose receivers must have a percentage).
- **Walkthrough:** `step_distribution` extended: Carla defines the key
  figure `EMPL` (employees, unit EA), posts 3 for CC-1100 and 1 for
  CC-1300 for the period, defines a distribution `SHARED-EMP` from the
  shared services whose shares are the key figure, Alex posts more
  shared supplies, and the run splits them 3 to 1, rounded to the cent.
- **Acceptance:** a key figure's code is unique per company (409); its
  unit is a known unit of measure (422); a posting names a regular
  period of the company and active cost centers of its company, each
  once, with non-zero quantities (422 naming each line); a cycle by key
  figure names one of its company (422) and its receivers no percentage
  (422 `not_allowed`), a cycle by fixed percentages as before; a run
  reads the receivers' values of the period it is run for: a receiver
  whose value is negative refuses the run (409
  `controlling.allocation_key_figure_negative`), receivers whose values
  add up to nothing refuse it (409
  `controlling.allocation_key_figure_missing`), a receiver without a
  value takes nothing; the shares add up to each cost to the minor unit;
  the values used are in the run's audit record; schema tests for every
  new rule.
- **Depends on:** T60f, T60g.

### T60k Fixed amounts per receiver, and an allocation run reversed

**Capability.** A controller defines an assessment cycle whose
receivers are charged fixed amounts each period, and reverses a run of
any cycle in its period, after which the cycle may be run for the period
again.

- **Reuses:** T60g's assessment (one entry on the assessment account);
  `ledger.Postings.Reverse` (the run's entry reversed in every ledger it
  was posted to, on its own date), as T60i reverses a settlement; the
  lock on the sender's cycles (`heldCycle`).
- **Adds:** `share_basis` `fixed_amounts`, an assessment's alone, each
  receiver with its `amount` in the company's currency (above zero);
  table `controlling.allocation_run_reversal` (insert-only, one per
  run); the run's `attempt` within its cycle and period, so that one run
  per cycle and period is in force; `POST /v1/allocation-runs/{id}:reverse`;
  the reversal on `AllocationRun`; event `AllocationRunReversed`; the
  ledger's `:reverse` of a run's entry names the path.
- **Walkthrough:** `step_assessment` extended: Carla reverses the
  `FACILITIES` run (the receivers' charges gone, the sender's credit
  gone), defines `FAC-FIXED` charging CC-1100 CAD 250.00 and CC-1300
  CAD 150.00 from the facilities, and runs it for the period: CAD
  400.00 charged; a second reversal of the first run is refused.
- **Acceptance:** a fixed-amounts cycle is an assessment (422
  `not_allowed` on a distribution), each receiver with an amount above
  zero in the company's currency and no percentage (422 naming the
  field); its run charges each receiver its amount and credits the
  sender with their total whatever the sender's costs, posts even when
  the sender has none, and reads no cost (`account_count` 0); a run is
  reversed once (409 `controlling.allocation_run_reversed`), on its own
  posting date, so its period must still be open (the ledger's 409
  `ledger.period_not_open`), at the cycle's version (412); a reversed run
  no longer counts as the cycle's run of the period nor as the sender's
  assessment of it, so either cycle may run again; a reversal and a run
  of one sender follow one another under the sender's cycles' lock;
  every reversal audited and published; schema tests.
- **Depends on:** T60j.

**Later (still in the placeholder):** shares from actual or plan values;
fixed portions; fixed values of a key figure carried to later periods;
several senders and segments in a cycle; iterative cycles; simulating a
run; plan allocations; overhead rates and activity allocation; receivers
other than cost centers; fixed amounts on a distribution (per account);
reversing into a later open period when the run's period is closed; a
lock of a period for statistical key figure postings; changing or
deleting a key figure.
