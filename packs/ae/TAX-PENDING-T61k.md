# UAE pack: the VAT 201 pending the tax specialist (T61k)

`2026.2/returns.yaml` defines the return type `VAT201`: the VAT 201's
boxes, computed from the tax items of the companies a UAE registration
covers (a tax group's members included, ADR-0049 §10). 2026.2 is 2026.1
plus that file and carries no `sign_off`, so production refuses to apply
it until the tax specialist has confirmed what this file lists, with the
items of `TAX-PENDING-T62f.md`; the confirmation is a later version
(2026.2 plus `sign_off`, or with what the specialist corrects), as Canada
2026.4's codes were signed off as 2026.5 (T62h). A test
(`internal/countrypack`, TestUAEPackVAT201) reads this table and the pack
and fails when the boxes, their order, what they compute or their basis
differ, so keep the table's layout.

Every box is as ADR-0049 records it (Context, "VAT 201 boxes"; §9; §11)
from the FTA's VAT return user guide (v. 2021, before EmaraTax) **as
summarised by Zoho and ClearTax, secondary sources**: ADR-0049 says "secondary
sources agree, but the guide predates EmaraTax, so the live portal's
labels must be checked". Nothing was fetched again in T61k.

Basis: `ADR-0049` is a box and column the ADR records (1a-1g with amount,
VAT and adjustment; 9 with amount, recoverable VAT and adjustment; 10
with recoverable VAT; 14 = 12 - 13, §11). `ADR-0049 box` is a box the ADR
names without its columns (2-7): which of amount and VAT the box has is
the pack's reading, and its label says the column is pending. `memory`
is a total the ADR names ("8 totals", "11 totals", "12 total due tax",
"13 total recoverable tax") without its sum, written from memory: its
label says so.

Computed as: for a sum of tax items, `direction · kinds · components ·
jurisdictions · account keys · measure`, `-` where the box does not
select by it, and `shown positive` where the sum is negated (output tax
and its base are credits); for a total, `=` and its expression over
earlier boxes. Amounts are in AED, the registrant's local currency.

| Box | Code | Computed as | Basis |
|---|---|---|---|
| 1a | 1a_amount | output · standard · vat · AE-AZ · output · base · shown positive | ADR-0049 |
| 1a | 1a_vat | output · standard · vat · AE-AZ · output · tax · shown positive | ADR-0049 |
| 1a | 1a_adjustment | output · adjustment · vat · AE-AZ · - · tax · shown positive | ADR-0049 |
| 1b | 1b_amount | output · standard · vat · AE-DU · output · base · shown positive | ADR-0049 |
| 1b | 1b_vat | output · standard · vat · AE-DU · output · tax · shown positive | ADR-0049 |
| 1b | 1b_adjustment | output · adjustment · vat · AE-DU · - · tax · shown positive | ADR-0049 |
| 1c | 1c_amount | output · standard · vat · AE-SH · output · base · shown positive | ADR-0049 |
| 1c | 1c_vat | output · standard · vat · AE-SH · output · tax · shown positive | ADR-0049 |
| 1c | 1c_adjustment | output · adjustment · vat · AE-SH · - · tax · shown positive | ADR-0049 |
| 1d | 1d_amount | output · standard · vat · AE-AJ · output · base · shown positive | ADR-0049 |
| 1d | 1d_vat | output · standard · vat · AE-AJ · output · tax · shown positive | ADR-0049 |
| 1d | 1d_adjustment | output · adjustment · vat · AE-AJ · - · tax · shown positive | ADR-0049 |
| 1e | 1e_amount | output · standard · vat · AE-UQ · output · base · shown positive | ADR-0049 |
| 1e | 1e_vat | output · standard · vat · AE-UQ · output · tax · shown positive | ADR-0049 |
| 1e | 1e_adjustment | output · adjustment · vat · AE-UQ · - · tax · shown positive | ADR-0049 |
| 1f | 1f_amount | output · standard · vat · AE-RK · output · base · shown positive | ADR-0049 |
| 1f | 1f_vat | output · standard · vat · AE-RK · output · tax · shown positive | ADR-0049 |
| 1f | 1f_adjustment | output · adjustment · vat · AE-RK · - · tax · shown positive | ADR-0049 |
| 1g | 1g_amount | output · standard · vat · AE-FU · output · base · shown positive | ADR-0049 |
| 1g | 1g_vat | output · standard · vat · AE-FU · output · tax · shown positive | ADR-0049 |
| 1g | 1g_adjustment | output · adjustment · vat · AE-FU · - · tax · shown positive | ADR-0049 |
| 2 | 2_vat | output · adjustment · tourist_refund · - · - · tax · shown positive | ADR-0049 box |
| 3 | 3_amount | input · reverse_charge · vat_due · - · self_output · base | ADR-0049 box |
| 3 | 3_vat | input · reverse_charge · vat_due · - · self_output · tax · shown positive | ADR-0049 box |
| 4 | 4_amount | output · zero · vat · - · - · base · shown positive | ADR-0049 box |
| 5 | 5_amount | output · exempt · vat · - · - · base · shown positive | ADR-0049 box |
| 6 | 6_amount | input · import · vat_due · - · self_output · base | ADR-0049 box |
| 6 | 6_vat | input · import · vat_due · - · self_output · tax · shown positive | ADR-0049 box |
| 7 | 7_vat | output · adjustment · import_adjustment · - · - · tax · shown positive | ADR-0049 box |
| 8 | 8_amount | = box_1a_amount + box_1b_amount + box_1c_amount + box_1d_amount + box_1e_amount + box_1f_amount + box_1g_amount + box_3_amount + box_4_amount + box_5_amount + box_6_amount | memory |
| 8 | 8_vat | = box_1a_vat + box_1b_vat + box_1c_vat + box_1d_vat + box_1e_vat + box_1f_vat + box_1g_vat + box_2_vat + box_3_vat + box_6_vat + box_7_vat | memory |
| 8 | 8_adjustment | = box_1a_adjustment + box_1b_adjustment + box_1c_adjustment + box_1d_adjustment + box_1e_adjustment + box_1f_adjustment + box_1g_adjustment | memory |
| 9 | 9_amount | input · standard · vat · - · input · base | ADR-0049 |
| 9 | 9_vat | input · standard · vat · - · input · recoverable | ADR-0049 |
| 9 | 9_adjustment | input · adjustment · vat · - · - · tax | ADR-0049 |
| 10 | 10_vat | input · reverse_charge, import · vat · - · self_input · recoverable | ADR-0049 |
| 11 | 11_amount | = box_9_amount | memory |
| 11 | 11_vat | = box_9_vat + box_10_vat | memory |
| 11 | 11_adjustment | = box_9_adjustment | memory |
| 12 | 12 | = box_8_vat + box_8_adjustment | memory |
| 13 | 13 | = box_11_vat + box_11_adjustment | memory |
| 14 | 14 | = box_12 - box_13 | ADR-0049 |

## For the tax specialist

The boxes:
1. The labels on EmaraTax: ADR-0049 records the boxes from the 2021 guide
   through secondary summaries, and the guide predates EmaraTax.
2. The columns of boxes 2-7 (`ADR-0049 box` above): 2 and 7 as VAT alone,
   3 and 6 as amount and VAT, 4 and 5 as amount alone (their rate is 0).
3. The totals' sums (`memory` above): 8's amount over 1a-1g, 3, 4, 5 and
   6; 8's VAT over 1a-1g, 2, 3, 6 and 7; 8's adjustment over 1a-1g; 11's
   amount and adjustment as 9's, its VAT 9's and 10's; 12 as 8's VAT and
   adjustment; 13 as 11's VAT and adjustment. 14 is ADR-0049's 12 - 13, a
   negative figure refundable.
4. The letters a-g follow the order in which ADR-0049 lists the emirates
   (Abu Dhabi, Dubai, Sharjah, Ajman, Umm Al Quwain, Ras Al Khaimah,
   Fujairah). Their ISO 3166-2 codes, which the sales lines name, are
   written from memory (AE-AZ, AE-DU, AE-SH, AE-AJ, AE-UQ, AE-RK, AE-FU;
   the repository's tests already used AE-DU and AE-AZ): a line naming
   another subdivision of AE is on no box, and its item stays unreported
   (the ledger checks only that it is a subdivision of AE).

How the tenant's codes reach the boxes (the pack's mapping, decided by
default in ADR-0049 §11 As built, T61k):
5. The emirate of a standard-rated supply is the one its S5 line names,
   which whoever posts the sale states as the emirate of the fixed
   establishment making it (ADR-0049 Context, §7 As built); the ERP holds
   no fixed establishments.
6. Box 3 takes the reverse charge on imported services and goods imported
   without a customs declaration (RC5) and the domestic reverse charge
   (DR5), as ADR-0049 §9 As built reports DR5 in box 3; the Context lists
   only imports for box 3. Both are kind `reverse_charge`, so they cannot
   be split without a component of their own.
7. Box 6 takes import VAT from a customs declaration's entry (IM5, the
   declared value its amount); box 10 takes the part recovered of both
   the reverse charge's and an import's claim (ADR-0049 records box 10
   for the reverse charge and "output and input" for box 6, not where
   an import's input goes).
8. Box 9 takes standard-rated purchases (V5 and VB5): VB5's blocked VAT
   recovers nothing, so it adds its amount and no VAT. Whether a blocked
   expense's amount belongs in box 9 is the specialist's.
9. Adjustments are manual entries of a code of kind `adjustment` (rate 0,
   its tax what the entry posts, as T61j built them for Canada), which the
   tenant adds: an output code of component `vat` levying the line's
   emirate (box 1's adjustment column, by emirate), an input code of
   component `vat` (box 9's), and output codes of components
   `tourist_refund` (box 2) and `import_adjustment` (box 7). A tourist
   refund posted as a debit to VAT payable shows negative in box 2 and
   reduces 8's VAT; which entries a business makes on boxes 2 and 7 is
   the specialist's to list.
10. Out-of-scope supplies (OS) are on no box; sales posted without a VAT
    code are on none either.

The return's rules:
11. The prior-period threshold of AED 10,000 of tax (P7, product owner
    2026-10-20, from ADR-0049's Context "as I recall; confirm"): its
    measure is open (net or gross, per period or pooled; ADR-0049 §11 As
    built, T61h), and whether it holds after the 2026 procedures
    amendments.
12. The amendment carries both the corrected figures and the differences
    (ADR-0049 §11 As built, T61i): which the FTA takes in a voluntary
    disclosure.
13. Due dates (28 days after the period's end, ADR-0049 Context) are not
    built for any authority (§10 As built).

Sources (in `2026.2/pack.yaml`):
- VAT 201 guide, as summarised by Zoho (secondary): https://www.zoho.com/ae/books/vat/guides/vat-return.html
- VAT 201 guide, as summarised by ClearTax (secondary): https://cleartax.com/ae/vat-return-uae
