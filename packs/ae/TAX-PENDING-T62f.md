# UAE pack: the tax facts and what waits for the tax specialist (T62f)

`2026.1/tax.yaml` writes the codes below and the tax settings after them.
2026.1 carries no `sign_off`, so production refuses to apply it until the
tax specialist has confirmed what this file lists; the confirmation is a
later version (2026.1 plus `sign_off`, or with what the specialist
corrects), as Canada 2026.4 was signed off as 2026.5. A test
(`internal/countrypack`, TestUAEPackTaxCodes) reads this table and the
pack and fails when they differ, so keep the table's layout.

Every fact is as ADR-0049 records it (Context, §6, §7, §9, Sources), from
its proposal of 2026-10-20; nothing was fetched again in T62f, and nothing
here is stated from memory. ADR-0049 names the VAT Law (Federal
Decree-Law No. 8 of 2017) and its Executive Regulation (Cabinet Decision
No. 52 of 2017) without a link, and read the regulation's articles through
secondary summaries: the facts resting on them are marked so below, and
the pack's manifest lists only the sources ADR-0049 links.

Components are `key account_key account recoverable_percent`, `on line`
where the component levies the emirate each line names; every component's
jurisdiction is AE, and every rate the code's.

| Code | Supply | Direction | Kind | Category | Rate | In force | Components | Basis |
|---|---|---|---|---|---|---|---|---|
| V5 | standard-rated purchase | input | standard | S | 5% | from 2026-01-01 (earlier start not sourced) | vat input 1300 100 | VAT Law, standard rate 5% (ADR-0049 Context, no link); input tax recoverable (Context, Input tax) |
| VB5 | purchase whose input tax is blocked | input | standard | S | 5% | from 2026-01-01 (earlier start not sourced) | vat input 1300 0 | blocked for entertainment, motor vehicles available for personal use and employees' personal goods and services (Executive Regulation Art. 53, secondary summaries, ADR-0049 Context); a fixed share of 0 (P8) |
| RC5 | imported services, goods imported without a customs declaration | input | reverse_charge | AE | 5% | from 2026-01-01 (earlier start not sourced) | vat_due self_output 2300 100; vat self_input 1300 100 | reverse charge, boxes 3 and 10 (ADR-0049 Context, §9; VAT 201 guide, secondary); no self-invoice from 2026 (WAM, Federal Decree-Law No. 16 of 2025) |
| DR5 | domestic reverse charge: precious metals and stones, crude oil and natural gas, electronic devices | input | reverse_charge | AE | 5% | from 2026-01-01 (earlier start not sourced) | vat_due self_output 2300 100; vat self_input 1300 100 | Cabinet Decisions No. 91 of 2023 and No. 127 of 2024 (ADR-0049 Context; FTA clarification, WAM) |
| IM5 | goods imported through a customs declaration | input | import | - | 5% | from 2026-01-01 (earlier start not sourced) | vat_due self_output 2300 100; vat self_input 1300 100 | box 6, output and input, posted from the declaration (ADR-0049 §9; VAT 201 guide, secondary) |
| S5 | standard-rated supply, by emirate | output | standard | S | 5% | from 2026-01-01 (earlier start not sourced) | vat output 2300 100 on line | VAT Law, standard rate (ADR-0049 Context, no link); boxes 1a-1g by emirate at one rate (Context, §7 As built) |
| Z0 | zero-rated supply | output | zero | Z | 0% | from 2026-01-01 (earlier start not sourced) | vat output 2300 100 | the zero rate and its supplies (VAT Law, ADR-0049 Context, no link); box 4 |
| E0 | exempt supply | output | exempt | E | 0% | from 2026-01-01 (earlier start not sourced) | vat output 2300 100 | exempt supplies (VAT Law, ADR-0049 Context, no link); box 5 |
| OS | out of scope, such as goods within or between designated zones | output | out_of_scope | O | 0% | from 2026-01-01 (earlier start not sourced) | vat output 2300 100 | out of scope (ADR-0049 Context); designated zones (FTA Designated Zones VAT Guide; Cabinet Decision No. 59 of 2017 and Executive Regulation Art. 51, no link) |

The category column is the UN/CEFACT 5305 category ADR-0049 §1 gives the
UAE's codes (S, Z, E, O, AE), each code taking the one its kind names;
T65's e-invoice reads it.

The tax settings (`tax_settings`):
- Documentary requirement: above AED 10,000 (`above: "10000"`, a total
  equal to it not in the tier), the supplier's TRN (`supplier_tax_id`),
  pending sign-off: ADR-0049 §6 As built, from the Executive Regulation's
  Art. 59 as ADR-0049 Context records it (a simplified tax invoice where
  the recipient is registered and the supply is AED 10,000 or less;
  secondary summaries, no link).
- `rate_type: average`, the tenant recording the UAE Central Bank's rate
  under it: ADR-0049 Context cites VAT Law Art. 69 from memory ("as I
  recall it; confirm").
- `rounding: document`: ADR-0049 records no UAE rule ("UAE rounding" is
  pending, Sources, As built T62h).

## For the tax specialist

The rates and codes:
1. The standard rate of 5% and the zero, exempt and out-of-scope
   categories as ADR-0049 records them from the VAT Law, whose text the
   repository does not hold; the rate's start before 2026-01-01 (the pack
   starts every rate on 2026-01-01, so a document dated earlier finds no
   rate).
2. The codes chosen: one standard purchase code, one blocked (which of
   Art. 53's purchases a business codes VB5 is its own choice per
   purchase), the reverse charge for imported services and for the
   domestic supplies apart (DR5, so the return can tell them apart if it
   must), import VAT from the customs declaration, one standard output
   code for every emirate (each line names its emirate's ISO 3166-2
   code, such as AE-DU; the ledger checks only that it is a subdivision
   of AE, and from 2026.3 that it is one of the seven emirates S5 lists,
   the emirate guard), and zero-rated, exempt and
   out-of-scope output codes; no zero-rated or exempt purchase code.
3. Recovery: input VAT, reverse charge and import VAT wholly recovered on
   1300 (subject to the six-month payment rule of VAT Law Art. 55, which
   ADR-0049 records "as I read it; confirm" and the ERP does not check);
   blocked input recovering nothing, on the purchase's own line.
4. The UN/CEFACT categories above, and none for import VAT.

The tax settings:
5. The documentary tier: whether the test is the invoice's gross total
   (as the ERP measures every tier, in AED at the tax date's rate) or the
   supply's value, and whether the TRN is the only particular to check
   above AED 10,000 (the recipient's TRN, the tax in AED and the other
   particulars of Art. 59 are on the document only).
6. Which of a supplier's tax identifiers count as its TRN: the ERP takes
   any scheme named after the UAE (`AE_...`, ADR-0049 §6 As built).
7. The rate type (VAT Law Art. 69) and rounding per document.

Not in the pack, waiting for the specialist or another step:
8. The VAT 201 (T61k), its boxes, the AED 10,000 correction threshold
   (P7) and the scope of the 2026 five-year limit.
9. The UAE's public holidays and the company's weekend (no source is
   recorded; ADR-0048, Consequences).
10. Apportionment for a business that also makes exempt supplies (P8 (b),
    a later step).

Sources (in `2026.1/pack.yaml`):
- MoF VAT amendments from 2026-01-01 (WAM): https://www.wam.ae/en/article/bn10b09-ministry-finance-implement-vat-law-amendments
- FTA Designated Zones VAT Guide: https://u.ae/-/media/Documents-2023/Designated-Zones-VAT-Guide.ashx
- FTA clarification, reverse charge for electronic devices (WAM): https://wam.ae/en/details/1395303213742
- VAT 201 guide, as summarised by Zoho (secondary): https://www.zoho.com/ae/books/vat/guides/vat-return.html
- VAT 201 guide, as summarised by ClearTax (secondary): https://cleartax.com/ae/vat-return-uae
