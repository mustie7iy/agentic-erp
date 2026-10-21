# Canada pack: tax codes pending T61a

Not part of the pack (`2026.1/tax.yaml` has none): the GST/HST, QST and PST
codes sourced for T62e, to be written in the tax code schema of ADR-0049
(T61a). Purchase side; the table is from the 2026-10-20 retrieval of the
cited pages.

| Code | Tax | Rate | In force | Recovery (as proposed) | Source |
|---|---|---|---|---|---|
| G5 | GST (AB, BC, MB, NT, NU, QC, SK, YT) | 5% | from 2013-04-01 | input tax credit, account 1300 | CRA GST/HST rate table |
| HON | HST Ontario | 13% | from 2013-04-01 | input tax credit, 1300 | CRA rate table |
| HNB | HST New Brunswick | 13% / 15% | 2013-04-01..2016-06-30 / from 2016-07-01 | input tax credit, 1300 | CRA rate table; CRA GI-190 |
| HNL | HST Newfoundland and Labrador | 13% / 15% | 2013-04-01..2016-06-30 / from 2016-07-01 | input tax credit, 1300 | CRA rate table; CRA GI-190 |
| HNS | HST Nova Scotia | 15% / 14% | 2013-04-01..2025-03-31 / from 2025-04-01 | input tax credit, 1300 | CRA rate table |
| HPE | HST Prince Edward Island | 14% / 15% | 2013-04-01..2016-09-30 / from 2016-10-01 | input tax credit, 1300 | CRA rate table |
| QST | Quebec sales tax | 9.975% | from 2026-01-01 (earlier start not sourced) | input tax refund, 1310 | Revenu Québec, Calculating the Taxes |
| PBC | British Columbia PST | 7% | from 2026-01-01 (earlier start not sourced) | none (expensed, 6400) | gov.bc.ca PST |
| PMB | Manitoba RST | 7% | from 2026-01-01 (earlier start not sourced) | none (expensed, 6400) | gov.mb.ca Retail Sales Tax |
| PSK | Saskatchewan PST | 6% | from 2026-01-01 (earlier start not sourced) | none (expensed, 6400) | saskatchewan.ca PST |

Sources (also in `2026.1/pack.yaml`; keep them in the version T61a makes):
- CRA rate table: https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/charge-collect-which-rate/calculator.html
- CRA GI-190: https://www.canada.ca/en/revenue-agency/services/forms-publications/publications/gi-190/new-brunswick-newfoundland-labrador-hst-rate-increases-sales-rentals-new-housing.html
- Revenu Québec, Calculating the Taxes: https://www.revenuquebec.ca/en/businesses/consumption-taxes/gsthst-and-qst/collecting-gst-and-qst/calculating-the-taxes/
- B.C. PST: https://www2.gov.bc.ca/gov/content/taxes/sales-taxes/pst
- Manitoba Retail Sales Tax: https://gov.mb.ca/finance/taxation/taxes/retail.html
- Saskatchewan PST: https://sets.saskatchewan.ca/rptp/portal/footer/taxinformation/provincial-sales-tax/

For the tax specialist: the PST and QST start dates (reported elsewhere, not
fetched from an authority: QST 9.975% from 2013-01-01, BC PST 7% from
2013-04-01, MB RST 7% from 2019-07-01, SK PST 6% from 2017-03-23); whether
PST is expensed and QST recovered as written; purchases in BC, MB, SK and
QC carry GST and a provincial tax together.
