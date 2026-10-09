# SAP capability map

Every major capability across SAP S/4HANA and SAP's cloud products (Ariba,
SuccessFactors, Concur, Fieldglass, IBP, EWM, TM, GTS, MDG, GRC, Analytics
Cloud, Signavio, BTP), with how each one is replaced and in which phase.
Phases are defined in [plan.md](plan.md).

**Approaches**

| Approach | Meaning |
|---|---|
| Native core | Built as deterministic Go services in `internal/core` and its successors |
| Agent-native | Delivered mainly by agents working over core services |
| Integrate | Connect a specialist product instead of building (payroll, tax calculation, CRM, T&E...) |
| Industry pack | Configuration, data model extensions and agents built on demand for an industry |

The list was compiled from SAP's published portfolio and should be checked
against the client's licensed and activated scope during discovery.

120 capabilities: 64 native core, 38 agent-native, 17 integrate, 1 industry pack.

### Finance (20)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| General ledger and universal journal | FI-GL · ACDOCA | Native core | 0 | Done |
| Accounts payable | FI-AP | Native core | 1 | Done |
| Payments and bank communication | F110 · BCM | Native core | 1 | Done |
| Accounts receivable | FI-AR | Native core | 3 | Planned |
| Asset accounting | FI-AA | Native core | 2 | Planned |
| Bank accounting and statement reconciliation | FI-BL · FEBAN | Agent-native | 2 | Planned |
| Cash management and liquidity forecast | Cash Management | Agent-native | 2 | Planned |
| Financial close | Advanced Financial Closing | Agent-native | 2 | Planned |
| Group reporting and consolidation | Group Reporting · EC-CS | Native core | 2 | Planned |
| Parallel ledgers (IFRS, local GAAP) | Ledgers 0L/2L | Native core | 2 | Planned |
| Multi-currency and FX valuation | FAGL_FCV | Native core | 2 | Planned |
| Tax determination and calculation | FI-TAX | Integrate | 2 | Planned |
| E-invoicing and statutory reporting | Document & Reporting Compliance | Integrate | 2 | Planned |
| Revenue recognition (IFRS 15) | RAR | Native core | 3 | Planned |
| Credit management | FSCM-CR | Agent-native | 3 | Planned |
| Collections and dispute management | FSCM-COL · FSCM-DM | Agent-native | 3 | Planned |
| Treasury and risk management | TRM | Integrate | 2 | Planned |
| Lease accounting (IFRS 16) | RE-FX contracts | Native core | 6 | Planned |
| Travel and expense | Concur | Integrate | 8 | Planned |
| Funds management and public budgeting | PSM-FM | Industry pack | 10 | Planned |

### Controlling (7)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Cost center accounting | CO-OM-CCA | Native core | 2 | Planned |
| Profit center accounting | EC-PCA | Native core | 2 | Planned |
| Internal orders | CO-OM-OPA | Native core | 2 | Planned |
| Overhead allocation and settlement | KSU5 · KO88 | Native core | 2 | Planned |
| Margin and profitability analysis | CO-PA | Native core | 2 | Planned |
| Product costing and actual costing | CO-PC · Material Ledger | Native core | 5 | Planned |
| Budgeting and financial planning | SAC Planning · BPC | Agent-native | 9 | Planned |

### Sourcing and procurement (12)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Purchase requisitions and approvals | MM-PUR · ME51N | Native core | 1 | Done |
| Purchase orders | MM-PUR · ME21N | Native core | 1 | Done |
| Contracts and scheduling agreements | Outline agreements | Native core | 1 | Done |
| Goods receipt | MIGO · MSEG 101 | Native core | 1 | Done |
| Invoice verification and three-way match | MM-IV · MIRO | Native core | 1 | Done |
| Invoice capture from PDF and email | VIM · OCR | Agent-native | 1 | Done |
| Supplier onboarding and management | Ariba SLP · Business Partner | Agent-native | 1 | Done |
| Sourcing, RFQ and bid analysis | Ariba Sourcing | Agent-native | 4 | Planned |
| Catalogs and guided buying | Ariba Guided Buying | Integrate | 4 | Planned |
| Supplier network (cXML, EDI) | Ariba Network | Integrate | 4 | Planned |
| Services and contingent workforce | Fieldglass | Integrate | 8 | Planned |
| Spend analytics | Ariba Spend Analysis | Agent-native | 9 | Planned |

### Sales and service (10)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Customer master | Business Partner · KNA1 | Native core | 3 | Planned |
| Quotations and sales orders | SD · VA01 | Native core | 3 | Planned |
| Pricing (condition technique) | SD-PR · T683 | Native core | 3 | Planned |
| Order intake from email, PDF and EDI | IDoc ORDERS | Agent-native | 3 | Planned |
| Billing | SD-BIL · VF01 | Native core | 3 | Planned |
| Rebates and settlement management | Settlement Management | Native core | 3 | Planned |
| Returns and credit memos | SD returns | Native core | 3 | Planned |
| CRM, CPQ and commerce | Sales Cloud · CPQ · Commerce | Integrate | 3 | Planned |
| Customer service and field service | CS · Field Service Management | Native core | 6 | Planned |
| Subscription and usage billing | BRIM | Native core | 7 | Planned |

### Supply chain (13)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Material master | MARA · MARC | Native core | 4 | Planned |
| Inventory management | MM-IM | Native core | 4 | Planned |
| Inventory valuation | MBEW · Material Ledger | Native core | 4 | Planned |
| Physical inventory and cycle counting | MI01 | Agent-native | 4 | Planned |
| Batch and serial number management | LO-BM · serial numbers | Native core | 4 | Planned |
| Stock transfers and intercompany | STO | Native core | 4 | Planned |
| Shipping and delivery | LE-SHP | Native core | 4 | Planned |
| Available-to-promise | aATP | Native core | 4 | Planned |
| MRP and replenishment | MRP Live | Agent-native | 4 | Planned |
| Demand forecasting and S&OP | IBP | Agent-native | 4 | Planned |
| Warehouse management (advanced) | EWM | Integrate | 4 | Planned |
| Transportation management | TM | Integrate | 4 | Planned |
| Global trade and customs | GTS | Integrate | 4 | Planned |

### Manufacturing (9)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| BOMs and routings | PP-BD | Native core | 5 | Planned |
| Production planning and capacity | PP · CRP | Agent-native | 5 | Planned |
| Discrete production orders | PP-SFC | Native core | 5 | Planned |
| Process manufacturing and recipes | PP-PI | Native core | 5 | Planned |
| Repetitive manufacturing and kanban | PP-REM · Kanban | Native core | 5 | Planned |
| Variant configuration | LO-VC | Native core | 5 | Planned |
| Subcontracting | MM subcontracting | Native core | 5 | Planned |
| Quality management | QM | Native core | 5 | Planned |
| Manufacturing execution | Digital Manufacturing | Integrate | 5 | Planned |

### Assets, service and EHS (7)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Plant maintenance | PM · EAM | Native core | 6 | Planned |
| Preventive and predictive maintenance | APM | Agent-native | 6 | Planned |
| Mobile maintenance | Service and Asset Manager | Native core | 6 | Planned |
| Real estate management | RE-FX | Native core | 6 | Planned |
| Environment, health and safety | EHS | Native core | 6 | Planned |
| Product compliance and safety data sheets | EHS Product Compliance | Integrate | 6 | Planned |
| Sustainability and ESG reporting | Sustainability Control Tower | Agent-native | 9 | Planned |

### Projects and R&D (5)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Project system and project costing | PS · WBS | Native core | 7 | Planned |
| Portfolio and project management | PPM | Agent-native | 7 | Planned |
| Engineering change management | PLM · ECM | Native core | 7 | Planned |
| Document management | DMS | Native core | 7 | Planned |
| Recipe development | PLM-RD | Native core | 7 | Planned |

### Human capital (7)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Core HR and org management | Employee Central · OM | Native core | 8 | Planned |
| Time and attendance | Time Management | Native core | 8 | Planned |
| Workforce scheduling | WFM | Agent-native | 8 | Planned |
| Payroll | Employee Central Payroll · PY | Integrate | 8 | Planned |
| Recruiting and onboarding | SuccessFactors Recruiting | Integrate | 8 | Planned |
| Learning | SuccessFactors Learning | Integrate | 8 | Planned |
| Performance, compensation and succession | SuccessFactors Talent | Integrate | 8 | Planned |

### Master data (4)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Business partner model | Business Partner | Native core | 1 | Done |
| Master data governance workflows | MDG | Agent-native | 1 | Done |
| Data quality and deduplication | Information Steward | Agent-native | 1 | Done |
| Archiving and data retention | ILM | Native core | 9 | Planned |

### GRC and security (6)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Audit trail and audit management | Audit Management | Native core | 0 | Done |
| Access control and segregation of duties | GRC Access Control | Native core | 1 | Done |
| Roles and authorizations | PFCG · IAG | Agent-native | 1 | Done |
| Process controls and continuous monitoring | GRC Process Control | Agent-native | 9 | Planned |
| Risk management | GRC Risk Management | Agent-native | 9 | Planned |
| Data privacy (blocking, deletion) | ILM · DPP | Native core | 9 | Planned |

### Analytics and planning (9)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| On-demand reports and dashboards | Embedded analytics · Analytics Cloud stories · ALV reports | Agent-native | 2 | Partly built |
| Semantic layer and governed metrics | CDS analytical views · BW InfoObjects | Native core | 2 | Partly built |
| Analytical store | BW/4HANA · Datasphere | Native core | 2 | Partly built |
| Drill-down and ad-hoc analysis | Analysis for Office · BEx | Agent-native | 2 | Planned |
| Saved, scheduled and shared reports | SAC scheduling · report distribution | Agent-native | 2 | Partly built |
| Export to Excel and PDF | Analysis for Office · broadcasting | Native core | 2 | Partly built |
| Proactive insights and anomaly alerts | Smart Insights · SAC alerts | Agent-native | 3 | Planned |
| Forecasting and what-if analysis | SAC Predictive · Smart Predict | Agent-native | 9 | Planned |
| Process mining | Signavio | Agent-native | 2 | Planned |

### Platform (11)

| Capability | SAP equivalent | Approach | Phase | Status |
|---|---|---|---|---|
| Enterprise structure configuration | IMG enterprise structure | Agent-native | 1 | Done |
| Workflow and approvals | Flexible Workflow | Native core | 1 | Done |
| Output management (forms, email, EDI) | Output Management · Adobe Forms | Agent-native | 1 | Done |
| Integration (APIs, IDoc, EDI, events) | Integration Suite · PI/PO | Agent-native | 3 | Planned |
| Extensibility (custom fields, rules, apps) | Key-user extensibility · BTP · ABAP | Native core | 2 | Planned |
| Localization and country versions | Country versions | Agent-native | 2 | Planned |
| Data migration | Migration Cockpit · LTMC | Agent-native | 0 | Done |
| Test automation | Cloud ALM · Tricentis | Agent-native | 1 | Done |
| Application lifecycle and operations | Solution Manager · Cloud ALM · Basis | Agent-native | 4 | Planned |
| Assistant | Joule | Agent-native | 1 | Partly built |
| User interface and mobile | Fiori | Native core | 1 | Planned |

### Industry solutions (phase 10, on demand)

Retail and fashion, consumer products, wholesale distribution, utilities, oil
and gas, mining, mill products, chemicals, life sciences (GxP), automotive,
industrial machinery, aerospace and defense, high tech, engineering and
construction, professional services, public sector, higher education,
healthcare, banking, insurance, telecommunications, media, travel and
transportation, sports and entertainment, agribusiness.
