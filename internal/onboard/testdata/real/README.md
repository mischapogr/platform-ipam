# Real-writer `.xlsx` fixtures (package D2)

Every file in this directory was written by real, independent spreadsheet
software -- never by this repository's code, and never hand-assembled XML
(that style of fixture lives in `../../xlsx_test.go`). They exist to verify
`internal/onboard/xlsx.go` against files an operator could plausibly hand
over, per `docs/WORK_PLAN.md` package D2 and
`docs/ONBOARDING_IMPORT.md` ("No real Excel or LibreOffice file has been
read; the `.xlsx` tests use hand-built workbooks" -- this directory closes
that gap for two Python writers and LibreOffice; **Microsoft Excel itself
was not used and is not verified** -- see "What was not verified" below).

Regenerating requires Docker; reading the committed files (`go test
./internal/onboard/...`) does not.

## Files, writer, and version

| File | Writer | Version | Generator |
| --- | --- | --- | --- |
| `openpyxl_main.xlsx` | openpyxl | 3.1.5 | `gen/gen_openpyxl.py` |
| `openpyxl_merged_header.xlsx` | openpyxl | 3.1.5 | `gen/gen_openpyxl.py` |
| `openpyxl_styled_date.xlsx` | openpyxl | 3.1.5 | `gen/gen_openpyxl.py` |
| `openpyxl_header_offset.xlsx` | openpyxl | 3.1.5 | `gen/gen_openpyxl.py` |
| `xlsxwriter_main.xlsx` | XlsxWriter | 3.2.9 | `gen/gen_xlsxwriter.py` |
| `xlsxwriter_formula_cached.xlsx` | XlsxWriter | 3.2.9 | `gen/gen_xlsxwriter.py` |
| `xlsxwriter_empty_column.xlsx` | XlsxWriter | 3.2.9 | `gen/gen_xlsxwriter.py` |
| `xlsxwriter_frozen_autofilter.xlsx` | XlsxWriter | 3.2.9 | `gen/gen_xlsxwriter.py` |
| `xlsxwriter_wide_column.xlsx` | XlsxWriter | 3.2.9 | `gen/gen_xlsxwriter.py` |
| `libreoffice_from_csv.xlsx` | LibreOffice (headless, `--convert-to xlsx`) | 25.8.7.3 580(Build:3), inside `linuxserver/libreoffice:latest` | `gen/gen_libreoffice.sh` + `gen/networks_for_libreoffice.csv` |

Versions were read from each tool's own version string at generation time
(`print(openpyxl.__version__)`, `print(xlsxwriter.__version__)`, `soffice
--version` inside the container) -- not assumed from a package name.

## What each file covers

`openpyxl_main.xlsx` and `xlsxwriter_main.xlsx` carry the same logical
NETWORKS table (header `Account ID, Region, CIDR, VPC ID, Name`) from two
independent writers, so their read results are directly comparable
(`xlsx_real_test.go`'s `wantMainNetworks()` is asserted against both):

- a normal row
- an account id stored as a **number** that lost its leading zeros (`12345678`)
- an account id stored as **text** with leading zeros preserved (`"000123456789"`)
- a 12-digit account id stored as a **number** (`123456789012`) -- both
  writers store this as plain digits in `<v>`, never scientific notation
  (see "Scientific notation" below)
- a CIDR cell holding two CIDRs separated by a line break inside the cell
- a name with a comma and a non-breaking space
- an entirely empty row in the middle (spreadsheet row 8) that both writers
  **omit from the sheet XML entirely** rather than emitting an empty `<row>`
  element -- the row after it (spreadsheet row 9) must still be numbered 9,
  not 8
- a second sheet ("Accounts") selected via `ReadOptions.Sheet`

The other files each isolate one more real-workbook feature:

- `openpyxl_merged_header.xlsx` -- a merged header cell (`D1:E1`); OOXML
  stores a value only in a merge's top-left cell.
- `openpyxl_styled_date.xlsx` -- a styled cell (bold + fill) and a
  date-formatted cell (Excel serial `45306` = 2024-01-15, format
  `MM/DD/YYYY`).
- `openpyxl_header_offset.xlsx` -- the header is genuinely not row 1:
  spreadsheet rows 1-2 were never written at all, so they are entirely
  absent from the XML, and the real header sits at row 3.
- `xlsxwriter_formula_cached.xlsx` -- a formula cell with an explicit cached
  value (`<c t="str"><f>...</f><v>...</v></c>` for a string result, plain
  `<f>+<v>` for a numeric one).
- `xlsxwriter_empty_column.xlsx` -- a column left entirely empty between two
  used columns.
- `xlsxwriter_frozen_autofilter.xlsx` -- a frozen pane and an autofilter
  (`<pane>`/`<autoFilter>` metadata the reader must ignore).
- `xlsxwriter_wide_column.xlsx` -- data in column `ZZ` (0-based index 701).
- `libreoffice_from_csv.xlsx` -- a CSV converted to `.xlsx` by LibreOffice
  itself, the form an operator most plausibly hands over. LibreOffice's own
  CSV importer applied automatic type detection and silently turned
  `012345678901` and `098765432109` into numbers, dropping a leading zero --
  reproducing the real-world damage `docs/ONBOARDING_IMPORT.md` section 4
  describes, from a genuinely independent tool rather than an assertion.

## Scientific notation

`docs/ONBOARDING_IMPORT.md` section 4 requires rejecting an account id in
"Excel scientific notation" (`1.23457E+11`) as an error, because the digits
are already lost. All three writers checked here store a 12-digit integer
account id as plain digits in `<v>` (`123456789012`), never as scientific
notation -- Python doubles (and Calc's internal numeric type) represent an
integer that size exactly, so nothing forces a scientific rendering.
**Scientific notation in `<v>` was not reproduced by any real writer tested.**
The design's error path almost certainly exists for a CSV/paste origin
(where the *displayed* scientific-notation text gets typed or copied back
in as a literal string) rather than a genuine `.xlsx` numeric cell; the
existing hand-built-XML test in `../../xlsx_test.go` still covers the code
path directly, since Go's `encoding/xml` cannot tell a crafted `<v>` from a
"real" one.

## What was not verified

- **Microsoft Excel itself.** Only openpyxl, XlsxWriter and LibreOffice were
  used. Excel's own serializer may differ in ways these three don't
  exercise.
- Shared strings with `<rPh>` phonetic runs (a Japanese-furigana feature),
  cells of type `d` (ISO date) or `e` (error), and a relationship `Target`
  written as an absolute path (`/xl/worksheets/sheet1.xml`) rather than a
  relative one. None of the three writers checked produced any of these.
  `xlsx.go`'s `resolveSheetPath` already handles both relative and absolute
  targets defensively, and `cellValue`'s default branch already passes an
  unrecognized type's `<v>` through untouched, but neither path was
  exercised by a real file here.
- A `<c>` with no `r` attribute, and a `<row>` with no `r` attribute. Every
  writer checked always emits both. The existing fallback logic (`nextCol`
  in `rowsToGrid`, and `remapXLSXRowNumbers`'s "one past the previous row"
  rule) is exercised only by the pre-existing hand-built tests, not by a
  real file.
- A workbook `dimension` that disagrees with the actual data: the reader
  never parses `<dimension>` at all, so this cannot affect it either way,
  but that was not exercised by a real file with a deliberately wrong one.

## Regenerating

```sh
cd internal/onboard/testdata/real/gen
docker run --rm -v "$PWD":/out -w /out python:3.13-alpine sh -c \
  'pip install --quiet openpyxl && python gen_openpyxl.py'
docker run --rm -v "$PWD":/out -w /out python:3.13-alpine sh -c \
  'pip install --quiet xlsxwriter && python gen_xlsxwriter.py'
mv openpyxl_*.xlsx xlsxwriter_*.xlsx ..
sh gen_libreoffice.sh   # optional: pulls a ~2.5 GB image
```
