# Canada pack: the GST/HST return pending the tax specialist (T61j)

`2026.6/returns.yaml` defines the return type `GST34`: the GST/HST
return's lines, computed from the tax items of the companies a GST/HST
registration covers (ADR-0049 §11). 2026.6 carries no `sign_off`, so
production refuses to apply it until the tax specialist has confirmed what
this file lists; the confirmation is a later version (2026.6 plus
`sign_off`, or with what the specialist corrects), as 2026.4's codes were
signed off as 2026.5 (T62h). A test (`internal/countrypack`,
TestCanadaPackGST34) reads this table and the pack and fails when the
lines, their order or their basis differ, so keep the table's layout.

Basis: `ADR-0049` is the line's meaning as ADR-0049 (Context, "Return
lines") records it from the CRA's "Instructions for preparing a GST/HST
return" (source below; not fetched again in T61j). `memory` is a sum the
ADR does not state (it names 112, 113A, 113B and 113C a paper return's
subtotals only), written from memory: its label says so.

| Line | Box | Computed as | Basis |
|---|---|---|---|
| 101 | 101 | output items of kinds standard, zero, exempt, components gst and hst: their base, shown positive | ADR-0049 |
| 103 | 103 | output items of kind standard, components gst and hst, account key output: their tax, shown positive | ADR-0049 |
| 104 | 104 | output items of kind adjustment, components gst and hst: their tax, shown positive | ADR-0049 |
| 105 | 105 | 103 + 104 | ADR-0049 |
| 106 | 106 | input items of kinds standard, reverse_charge, import, components gst and hst, account keys input and self_input: the part recovered | ADR-0049 |
| 107 | 107 | input items of kind adjustment, components gst and hst: their tax | ADR-0049 |
| 108 | 108 | 106 + 107 | ADR-0049 |
| 109 | 109 | 105 - 108 | ADR-0049 |
| 110 | 110 | input items of kind adjustment, component instalment: their tax | ADR-0049 |
| 111 | 111 | input items of kind adjustment, component rebate: their tax | ADR-0049 |
| 112 | 112 | 110 + 111 | memory |
| 113A | 113a | 109 - 112 | memory |
| 205 | 205 | input items of kinds reverse_charge, import, components gst_205 and hst_205, account key self_output: their tax, shown positive | ADR-0049 |
| 405 | 405 | input items of kinds reverse_charge, import, components gst_405 and hst_405, account key self_output: their tax, shown positive | ADR-0049 |
| 113B | 113b | 205 + 405 | memory |
| 113C | 113c | 113A + 113B | memory |
| 114 | 114 | -113C when 113C is negative, else 0 | memory |
| 115 | 115 | 113C when 113C is positive, else 0 | memory |

## For the tax specialist

The lines:
1. The sums of 112, 113A, 113B and 113C, and 114 and 115 as the
   negative and positive 113C (from memory above). Whether 205 and 405
   come after 113A on the form, as the file orders them (an expression
   reads only earlier lines).
2. Line 101: whether out-of-scope supplies (kind `out_of_scope`) belong
   in it (left out), and that it holds only revenue posted with a GST/HST
   code: sales posted without a tax code are on no line.
3. Line 101 is in whole dollars on a paper return (ADR-0049 Context); the
   return here is in cents: is that how it is entered when filed online?
4. Line 205: whether its self-assessed tax is claimed back as an input
   tax credit on 106 like 405's (ADR-0049 §9 states it for 405 only); as
   built, 106 takes the recovered part of every gst or hst claim, so a
   code that should claim nothing has a recoverable share of 0.

How the tenant's codes reach the lines (the pack's mapping, decided by
default in ADR-0049 §11 As built, T61j):
5. GST and HST are the components keyed `gst` and `hst`, as the pack's
   purchase codes key them; a sales code, a zero-rated or exempt code
   names its component `gst` (the country's) or `hst` (the province's) so
   that 101 and 103 see it, and has one such component: 101 sums the base
   of each, so a code with both (an HST split into gst and hst parts)
   would count its base twice; nothing refuses such a code. QST and the
   provincial sales taxes are on no line, so a Quebec registrant's QST
   waits for the FPZ-500 below.
6. A self-assessed code keys its owed part (`self_output`) after its
   line (`gst_405`, `hst_405`; `gst_205`, `hst_205`) and its claim
   (`self_input`) `gst` or `hst`.
7. Adjustments, instalments and rebates are manual entries of a code of
   kind `adjustment` (rate 0, its tax what the entry posts): an output
   code increases the net tax (104), an input code decreases it (107),
   and input codes keyed `instalment` and `rebate` are 110 and 111. Which
   adjustments a business enters on 104 and 107 (the ITC restrictions of
   ADR-0049 §6, for one) is the specialist's to list for the pilot. An
   adjustment code is meant for a manual entry's tax line alone; only the
   capture refuses to propose one, and a sales or invoice line naming one
   is not refused: its base is then on no line (101 leaves the kind out).

Not built, waiting for the specialist and the pilot:
8. The FPZ-500 (GST/HST and QST) for a Quebec registrant (P4 (b): only if
   the pilot company is registered in Quebec). ADR-0049 records no line
   of it ("I have not found the FPZ-500 line list publicly", Unsure list),
   so none is written: the specialist states its lines from Revenu
   Québec's form, and the pilot its registrations.

Source (in `2026.6/pack.yaml`):
- CRA, Instructions for preparing a GST/HST return: https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/calculate-prepare-report/instructions-preparing-return.html

Signed off (`2026.7/pack.yaml`): the tax specialist, Musto Ibrahim,
confirmed the lines, the sums written from memory and the mapping above,
items 1-7, as written on 2026-11-09 (reported by the product owner).
2026.7's `returns.yaml` is 2026.6's byte for byte, its labels' "from
memory" markers included (ADR-0049 §11 As built, T61j: Signed off). Item
8, the FPZ-500, is not part of it and stays its own tracker leaf.
