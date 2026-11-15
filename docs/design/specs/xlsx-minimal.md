# Minimal SpreadsheetML workbook: what T95d needs from ECMA-376

Supplied for T95d (Excel workbook export) because build lanes do not fetch
(plan-breadth-week3.md, question 8). This file covers what is needed to write a one-sheet
workbook with `archive/zip` and `encoding/xml`. Every rule cites its source. The rules are
summarised in our own words, and quotations are short. Anything the sources do not settle is
marked **unconfirmed**.

## Sources (fetched 2026-11-15)

| Tag | Document | Where |
| --- | --- | --- |
| P1 | ECMA-376 Part 1, 5th edition (December 2016), "Fundamentals and Markup Language Reference" (= ISO/IEC 29500-1:2016) | ecma-international.org, `ECMA-376-1_5th_edition_december_2016.zip` |
| P2 | ECMA-376 Part 2, 5th edition (December 2021), "Open Packaging Conventions" | ecma-international.org, `ECMA-376-2_5th_edition_december_2021.zip` (PDF and `opc-contentTypes.xsd`, `opc-relationships.xsd`) |
| P4 | ECMA-376 Part 4, 5th edition (December 2016), "Transitional Migration Features" | ecma-international.org, `ECMA-376-4_5th_edition_december_2016.zip` (PDF and `OfficeOpenXML-XMLSchema-Transitional.zip`, of which `sml.xsd` is quoted) |
| OI | Microsoft [MS-OI29500] "Office Implementation Information for ISO/IEC 29500 Standards Support", v20260818 (release 18 August 2026) | learn.microsoft.com/openspecs/office_standards/ms-oi29500, PDF |
| XL | Microsoft Support, "Excel specifications and limits" | support.microsoft.com/office/excel-specifications-and-limits-1672b34d-7043-467e-8e27-269d656771c3 |
| XML | W3C, Extensible Markup Language (XML) 1.0, Fifth Edition | w3.org/TR/xml/ |

Part 1 defines the Strict conformance class, whose namespaces start
`http://purl.oclc.org/ooxml/`. Excel's usual files are Transitional. Part 4 §10.2 lists the
SpreadsheetML parts whose root namespace and relationship type differ in the Transitional
class. The content types are unchanged (P4 §10.1: only "source relationships and/or root
namespaces" differ). **This file uses the Transitional names throughout.**

## 1. Package (ZIP) rules

- The package is a ZIP file. The only compression methods allowed are 0 (stored) and 8
  (deflate). Encryption is not allowed (P2 §7.3.6; Annex B, Tables B.4 and B.5).
  `archive/zip`'s `Writer.Create` deflates (Go documentation). Data descriptors (flag bit 3)
  are allowed (P2 Annex B, Tables B.1 and B.5).
- ZIP item names are ASCII and unique. An item name is the part name without its leading `/`
  (P2 §7.3.3 and §7.3.4).
- Part names are matched ASCII case-insensitively (P2 §6.2.2.3).
- The content types stream is the ZIP item `[Content_Types].xml` (P2 §7.3.7).
- The package relationships part is `/_rels/.rels` (P2 §6.5.2.2). A part's relationships part
  is `<dir>/_rels/<name>.rels`; for example, `/xl/workbook.xml` has `/xl/_rels/workbook.xml.rels`
  (P2 §6.5.2.3).
- The XML of the OPC parts (content types and relationships) is UTF-8 or UTF-16, has no DTD,
  and is namespace-conformant (P2 §6.2.5).

## 2. Parts, content types and relationship types

| Part (ZIP item) | Content type | Relationship type (Transitional) pointing at it | Sources |
| --- | --- | --- | --- |
| `[Content_Types].xml` | (not a part) | none | P2 §7.2.3, §7.3.7 |
| `_rels/.rels` | `application/vnd.openxmlformats-package.relationships+xml` | none | P2 §6.5.2.1, §6.5.2.2 |
| `xl/workbook.xml` | `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml` | `http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument`, from `_rels/.rels` | P1 §12.3.23; P4 §10.2.23 |
| `xl/_rels/workbook.xml.rels` | `application/vnd.openxmlformats-package.relationships+xml` | none | P2 §6.5.2.3 |
| `xl/worksheets/sheet1.xml` | `application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml` | `http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet`, from the workbook | P1 §12.3.24; P4 §10.2.24 |
| `xl/styles.xml` | `application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml` | `http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles`, from the workbook | P1 §12.3.20; P4 §10.2.20 |

Rules:

- **Workbook.** A package has exactly one Workbook part, the target of a package
  relationship (P1 §12.3.23).
- **Worksheets.** Each worksheet is the target of an explicit relationship from the workbook,
  referenced by the `r:id` of its `sheet` element (P1 §12.3.24).
- **Styles.** There is at most one Styles part, the target of an implicit relationship from
  the workbook (P1 §12.3.20). It is needed because amounts use number formats, which only the
  Styles part holds (P1 §18.8.30 and §18.8.10).
- **Content types stream.** Every part except relationships parts needs a matching `Default`
  (by extension) or `Override` (by part name), and `Override` wins (P2 §7.2.3.2.1).
  - At most one `Default` per extension and one `Override` per part name. Order does not
    matter.
  - A `Default` may be declared even if no part uses it.
  - This file declares `Default Extension="rels"` with the relationships media type, as Excel
    files commonly do. P2 does not require it for relationships parts. **Unconfirmed** whether
    Excel needs it, so keep it.
- **Relationship attributes** (P2 §6.5.3.4):
  - `Id` is unique within the part (an `xsd:ID`).
  - `Type` is compared as a case-sensitive string.
  - `Target` is a relative reference, resolved against the source part.
  - `TargetMode` defaults to `Internal`.

## 3. Namespaces

| Prefix as used here | Namespace | Source |
| --- | --- | --- |
| (default) in content types | `http://schemas.openxmlformats.org/package/2006/content-types` | P2 §7.2.3.3; `opc-contentTypes.xsd` |
| (default) in `.rels` | `http://schemas.openxmlformats.org/package/2006/relationships` | P2 §6.5.2.1 |
| (default) in workbook, worksheet, styles | `http://schemas.openxmlformats.org/spreadsheetml/2006/main` | P4 §10.2.20, §10.2.23, §10.2.24; `sml.xsd` targetNamespace |
| `r:` (for `sheet/@r:id`) | `http://schemas.openxmlformats.org/officeDocument/2006/relationships` | P4 `sml.xsd` (`xmlns:r`), `shared-relationshipReference.xsd` |
| `xml:` (for `xml:space`) | `http://www.w3.org/XML/1998/namespace`, predeclared and never declared | P1 §18.3.1.96, §18.4.12 |

OI §2.1.11 (Part 1 §12.3.20, Styles Part) says Office requires a particular Styles root
namespace. Both URIs in that entry contain typing errors ("2006/mains" and "spreadhseetml"),
so this file relies on P4 §10.2.20 and `sml.xsd` for the namespace.

## 4. Minimal XML for every part

Element order matters, because the schema uses `xsd:sequence`.
- Workbook: `bookViews` comes before `sheets` (`CT_Workbook`).
- Worksheet: `sheetData` is the only required child (`CT_Worksheet`, `minOccurs="1"`).
- Styles: `numFmts`, `fonts`, `fills`, `borders`, `cellStyleXfs`, `cellXfs`, `cellStyles`,
  in that order (`CT_Stylesheet`).
All quoted from P4 `sml.xsd`.

`[Content_Types].xml`:

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
  <Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
</Types>
```

`_rels/.rels`:

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>
```

`xl/_rels/workbook.xml.rels`. Targets are relative to `/xl/workbook.xml`, as in the examples
of P1 §12.3.20 and §12.3.24:

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>
```

`xl/workbook.xml`. `bookViews` is optional in the schema, but OI §2.1.584(a) (Part 1 §18.2.27)
says "Excel will fail to print files without a bookViews element", so it is written.
`workbookView` has no required attributes (`CT_BookView`). `sheets` holds one or more `sheet`
elements (`CT_Sheets`). `sheet` requires `name`, `sheetId` and `r:id` (P1 §18.2.19;
`CT_Sheet`).

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
          xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <bookViews><workbookView/></bookViews>
  <sheets>
    <sheet name="Query" sheetId="1" r:id="rId1"/>
  </sheets>
</workbook>
```

`xl/worksheets/sheet1.xml`: a header row and one data row. A1 is text, and B2 is the amount
1234.567 in a currency with three decimals, using style 3 from the styles below.

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1">
      <c r="A1" t="inlineStr"><is><t>Customer</t></is></c>
      <c r="B1" t="inlineStr"><is><t>Amount</t></is></c>
    </row>
    <row r="2">
      <c r="A2" t="inlineStr"><is><t xml:space="preserve">=SUM(A1) </t></is></c>
      <c r="B2" s="3"><v>1234.567</v></c>
    </row>
  </sheetData>
</worksheet>
```

`xl/styles.xml`. There is one `xf` in `cellXfs` per number format used; a cell's `s` is the
zero-based index into `cellXfs`.

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <numFmts count="1">
    <numFmt numFmtId="164" formatCode="#,##0.000"/>
  </numFmts>
  <fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>
  <fills count="2">
    <fill><patternFill patternType="none"/></fill>
    <fill><patternFill patternType="gray125"/></fill>
  </fills>
  <borders count="1"><border/></borders>
  <cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
  <cellXfs count="4">
    <xf numFmtId="0"   fontId="0" fillId="0" borderId="0" xfId="0"/>
    <xf numFmtId="3"   fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
    <xf numFmtId="4"   fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
    <xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
  </cellXfs>
  <cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>
```

How each piece of the styles part is sourced:

- **Defined by the standard:** `s` indexes `cellXfs` (OI §2.1.590(g) and P1 §18.8.10). `xfId`
  indexes `cellStyleXfs` (P1 §18.8.45). `applyNumberFormat` says whether the xf's number
  format is applied (P1 §18.8.45).
- **Required by Office:** "All child elements that are referenced in the Excel document must
  be present" (OI §2.1.717, Part 1 §18.8.39). Every `fontId`, `fillId`, `borderId` and `xfId`
  above therefore points at an element that exists.
- **Taken from the standard's own example:** the two leading fills, `none` then `gray125`.
  They are the first two fills of the styles example in P1 Annex L.2.7.4.3, which is
  informative. **Unconfirmed** whether Excel rejects a file without them. The `cellStyles`
  entry and the font's `sz` and `name` are also not required by any cited rule. They are
  written because every style an Excel file references should exist (OI §2.1.717).
  **Unconfirmed** whether they can be left out.

## 5. sheetData, row and c

- `sheetData` holds the cell table, with cells grouped by row (P1 §18.3.1.80). It contains
  `row*` (`CT_SheetData`).
- `row` contains `c*` (`CT_Row`). Its `r` is the 1-based row index (P1 §18.3.1.73). Office
  requires `r` to be between 1 and 1048576 and "in non-decreasing order"
  (OI §2.1.644(a)). Excel's limit is 1,048,576 rows by 16,384 columns (XL).
- `c` (P1 §18.3.1.4; `CT_Cell`):
  - Its children are `f?`, `v?`, `is?`, `extLst?`, in that order.
  - Attributes:
    - `r`: an A1 reference (`ST_CellRef`).
    - `s`: style index, default 0.
    - `t`: cell type (`ST_CellType`), default `n`.
  - When `t` is `inlineStr`, "only the element `is` is allowed as a child element"
    (P1 §18.3.1.4).
  - `r` is optional. Without it, the cell goes one column right of the previous cell, or in
    the first column (OI §2.1.590(c)). Writing it on every cell avoids depending on that rule.
  - Office allows `s` from 0 to 65490 (OI §2.1.590(f)).
- **A1 references** (P1 §18.17.2.3.1):
  - The column is letters: A–Z, then AA–AZ, BA–BZ, …, ZA–ZZ, AAA… Column letters are not
    case-sensitive.
  - The row number follows the letters and is not zero.
  - So column n (1-based) is written in bijective base 26: `A`=1, `Z`=26, `AA`=27.
- **Cell types used** (P1 §18.18.11, `ST_CellType`):
  - `n` (Number) means the value is in `v`.
  - `inlineStr` (Inline String) means the value is in `is`, not `v`.
  - `str` (String) is a "Cell containing a formula string", meant for the string result of a
    formula in `f`. Do not use it for plain text: use `inlineStr`.
  - `s` (shared string) needs a Shared String Table part, which this minimal package does not
    have.
  - The Transitional `ST_CellType` has `b n e s str inlineStr`. It has no `d` (date), which
    Strict P1 §18.18.11 has (P4 `sml.xsd`).
- `is` (P1 §18.3.1.53; `CT_Rst`) holds `t?`, or rich-text runs `r*`. For plain text write
  `<is><t>…</t></is>`.
- `v` (P1 §18.3.1.96) holds the value directly. A formula cell's `v` holds the formula's last
  result.
- Cell text, whether in `t` or `v`, is at most 32,767 characters in Excel (OI §2.1.678(b),
  §2.1.660(a); XL).

## 6. Writing a number in `v`

- `v` is typed `ST_Xstring` in the schema (`CT_Cell`). For a number cell, Office requires "a
  textual representation of a double-precision floating point number" (OI §2.1.660(b)).
- **Lexical form** (P1 §18.17.5.3):
  - A base-10 mantissa, optionally followed by `E` or `e` and an integer exponent.
  - The mantissa is a decimal: digits, at most one `.`, and an optional leading sign. Leading
    and trailing zeros are optional.
  - Examples from the standard: `-1E4`, `1267.43233E12`, `12.78e-2`, `12`, `-0`.
  - There is no `INF` or `NaN` form. That differs from `xsd:double`, which has them; the
    standard defines its own lexical form here rather than citing `xsd:double`.
- **Interpretation** (P1 §18.17.5.4): the literal is read as mantissa × 10^exponent and stored
  as the nearest value in the value space. On a tie, the smaller magnitude wins. A magnitude
  above 2^1024 − 2^971 becomes `#NUM!`.
- **Value space**: binary64 by default (P1 §18.17.5.2). Office excludes denormals
  (OI §2.1.870), and treats a nonzero value smaller than 2^-1022 as 0 (OI §2.1.871(b)).
- **Precision**: Excel's "Number precision" is "15 digits" (XL). Excel also accepts typed
  numbers only up to ±9.99999999999999E+307 (XL).
- **Consequences for T95d.** These are inferences from the rules above:
  - Build the decimal string straight from `money.Minor` and the currency exponent, with no
    float in between. Insert the decimal point `exponent` places from the right, padding with
    zeros. Example: minor `-5`, exponent 3 gives `-0.005`; minor `123456`, exponent 2 gives
    `1234.56`.
  - Write no exponent, no `+` and no `-0`.
  - The file then holds the exact decimal. What Excel keeps is the nearest binary64 value,
    which Excel shows to 15 significant digits. An amount with more than 15 significant
    digits (in minor units, at least 10^15) may not show exactly in Excel.
- **Decimals shown**: the number format sets how many decimals Excel displays (section 9), not
  the text in `v`.

## 7. Text: XML characters, control characters, whitespace

- **Legal characters.** XML 1.0 allows only
  `Char ::= #x9 | #xA | #xD | [#x20-#xD7FF] | [#xE000-#xFFFD] | [#x10000-#x10FFFF]`
  (XML §2.2, production [2]). A character reference must also name a legal `Char`, so
  `&#x8;` is not well-formed either (XML §4.1, "WFC: Legal Character").
- **Escape for other characters**: `ST_Xstring` (P1 §22.9.2.19). A character that XML 1.0
  cannot represent is written `_xHHHH_`, with four hex digits; for example, U+0008 becomes
  `_x0008_`. The `t` element, `v`, the sheet name and `formatCode` are all `ST_Xstring`
  (P1 §18.4.12, §18.3.1.96, §18.2.19, §18.8.30).
- **What Office adds** (OI §2.1.1747, Part 1 §22.9.2.19):
  - Carriage return (U+000D) "shall be escaped" as `_x000D_`. An XML parser otherwise turns
    CR and CRLF into LF (XML §2.11).
  - Line feed and tab are escaped only inside attribute values, where XML attribute
    normalisation would turn them into spaces (XML §3.3.3). In element text such as `t`,
    write them as they are. Office does not allow LF or tab to be escaped in `ST_Xstring`
    content.
  - Text that already looks like `_xHHHH_` must have its leading underscore escaped as
    `_x005F_`, so that `_x0041_` is written `_x005F_x0041_`. Excel allows `_x005F_` only in
    that position.
- **Go's `encoding/xml`** (Go 1.26 source, `escapeText`):
  - It escapes `& < > " '`, writes tab, LF and CR as `&#x9;`, `&#xA;` and `&#xD;`, and
    **replaces every character outside `Char` with U+FFFD**, as it does invalid UTF-8.
  - To keep control characters as the standard's escapes, apply the `_xHHHH_` and `_x005F_`
    escaping first and XML-escape afterwards. Also decide whether CR becomes `_x000D_` (OI)
    rather than `&#xD;`.
  - **Unconfirmed**: how Excel treats `&#xD;` in `t` compared with `_x000D_`. OI asks for the
    latter.
- **Unpaired surrogates and invalid UTF-8** are not `Char`s and cannot be represented. The
  standard gives no rule for them; Go writes U+FFFD.
- **Whitespace.** `t` and `v` accept `xml:space` with values `default` or `preserve` (P1
  §18.4.12 and §18.3.1.96; XML §2.10). Excel uses it to say how whitespace in the element is
  handled (OI §2.1.678(a), §2.1.660(c)). Write `xml:space="preserve"` on any `t` whose text
  has leading or trailing whitespace, or a run of spaces, tabs or newlines. **Unconfirmed**
  exactly what Excel does with such text when the attribute is missing. Writing the
  attribute on every `t` is allowed.
- **Schema gap.** `xml:space` is in P1's prose, but the Transitional schema types `t` and `v`
  as the simple type `ST_Xstring`, which has no attributes. A strict XSD validation against
  P4 `sml.xsd` therefore rejects `xml:space` (checked with xmllint; see section 11). Office
  writes it (OI). A test that validates against `sml.xsd` must allow this one attribute.

## 8. Why an inline string is never a formula

- A formula exists only as the text of an `f` element (P1 §18.3.1.40: "The formula
  expression is contained in the character node of this element"). `v` holds a formula
  cell's last calculated result (P1 §18.3.1.96).
- A cell with `t="inlineStr"` may have only `is` as a child (P1 §18.3.1.4), so it cannot carry
  `f`. Its `is/t` is `ST_Xstring`, a string (P1 §18.4.12).
- Nothing in the cited text turns string content into a formula on load. So
  `<c t="inlineStr"><is><t>=HYPERLINK("…")</t></is></c>` is the text `=HYPERLINK("…")`.
  Values starting with `+`, `-` or `@` are text in the same way. This meets T95d's acceptance
  criterion when every text cell uses `inlineStr` and the writer never emits `f`.
- `quotePrefix` on an `xf` (P1 §18.8.45) marks text "prefixed by a single quote mark". It is
  not needed for the above.
- **Unconfirmed**: what Excel does if a user later edits such a cell and confirms it. That is
  user input, outside the file format.

## 9. Number formats

- **Mechanism.**
  - A cell's `s` selects an `xf` in `cellXfs` (OI §2.1.590(g); P1 §18.8.10).
  - The `xf`'s `numFmtId` selects the format (P1 §18.8.45).
  - A built-in ID needs no `numFmt` element. Its code is "implied rather than explicitly
    saved in the file" (P1 §18.8.30).
  - Any other ID needs `<numFmt numFmtId="…" formatCode="…"/>` inside `numFmts`
    (P1 §18.8.30, §18.8.31; `CT_NumFmt`, both attributes required).
- **Built-in IDs T95d needs** (P1 §18.8.30, "All Languages"; OI §2.1.712(a) changes none of
  these):

| ID | formatCode | Use |
| --- | --- | --- |
| 0 | `General` | Default; text cells |
| 1 | `0` | Integer, no thousands separator |
| 2 | `0.00` | Two decimals, no thousands separator |
| 3 | `#,##0` | Exponent 0 currency (e.g. JPY) |
| 4 | `#,##0.00` | Exponent 2 currency |

- **Custom formats.**
  - For any other number of decimals, write a custom code with that many `0`s after the
    point: exponent 3 (for example KWD or BHD) is `#,##0.000`, and exponent 1 is `#,##0.0`.
    `0` is a digit placeholder that shows insignificant zeros, `,` between placeholders is
    the thousands separator, and `.` is the decimal point (P1 §18.8.31, format symbols
    table).
  - The standard does not reserve a numeric range for custom IDs. It says IDs outside its
    lists have implementation-defined behaviour, and that "Ids not specified in the listing,
    such as 5, 6, 7, and 8, shall follow the number format specified by the formatCode
    attribute" (P1 §18.8.30).
  - The standard's own examples number custom formats 164 and up: 164 to 168 in P1
    §12.3.20, 164 in Annex L.2.7.4.3, and 166 in §18.8.31. Use 164, 165 and so on.
    **Unconfirmed** in the sources as a rule; it is the convention of the standard's
    examples.
- **Limits:** a `formatCode` is shorter than 255 characters (OI §2.1.712(b)). A workbook holds
  "between 200 and 250" number formats, depending on Excel's language version (XL).
- **Unconfirmed**: how a non-English Excel displays `,` and `.` in a stored code. OI says its
  grammar is for en-US and other locales need "international consideration" (OI §2.1.712(b)),
  but the sources read do not say how the stored file differs.

## 10. Sheet name

- The standard requires only that the name be unique (P1 §18.2.19). OI §2.1.578 (Part 1
  §18.2.19) adds Office's rules:
  - At most 31 characters.
  - It must "follow the character limitations for sheet-name and sheet-name-special in
    Formulas" (P1 §18.17).
  - Unique case-insensitively in any locale.
  - `sheetId` between 1 and 65534.
- The `sheet-name-special` grammar (P1 §18.17.2.3, the quoted-name form, which allows the most
  characters) excludes `'`, `*`, `[`, `]`, `\`, `:`, `/` and `?` as the first and last
  character. In between, the same characters are excluded except the apostrophe, which may
  appear in the middle (it is doubled when the name is quoted inside a formula).
- So a sheet name:
  - is 1–31 characters long;
  - contains none of `* [ ] \ : / ?`;
  - does not start or end with `'`.
  `Query` or `Report` qualifies. A name built from user text should be sanitised against
  these rules.
- **Unconfirmed**: whether "31 characters" counts UTF-16 code units or characters, and whether
  Excel reserves any names. Neither is in the sources read.

## 11. What was checked here, and what was not

- The six XML examples in section 4 were validated with `xmllint --schema` against the
  standard's own schemas: P2 `opc-contentTypes.xsd` and `opc-relationships.xsd`, and P4
  Transitional `sml.xsd`. All validate, except the `xml:space` attribute on `t` (section 7).
- **Not checked:** opening the resulting `.xlsx` in Excel. No Excel was available, and the
  sources describe the format, not whether Excel accepts a particular file. T95d's test reads
  the workbook back with its own reader. A manual open in Excel, LibreOffice or Numbers
  remains worth doing once.
