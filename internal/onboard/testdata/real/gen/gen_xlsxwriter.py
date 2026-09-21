#!/usr/bin/env python3
"""Generate .xlsx fixtures for platform-ipam internal/onboard D2 verification
using XlsxWriter (an independent, second writer implementation -- never reads
xlsx, and constructs the OOXML from scratch differently than openpyxl does).
Run inside python:3.13-alpine with `pip install xlsxwriter`.

Produces (in the current directory):
  xlsxwriter_main.xlsx               - the same logical Networks+Accounts
                                        table as openpyxl_main.xlsx, so the
                                        two writers are directly comparable.
  xlsxwriter_formula_cached.xlsx     - a formula cell with a cached value
                                        (write_formula's explicit cached
                                        value parameter).
  xlsxwriter_empty_column.xlsx       - a column left entirely empty between
                                        two used columns (D, between CIDR/C
                                        and VPC ID/E).
  xlsxwriter_frozen_autofilter.xlsx  - a frozen pane and an autofilter over
                                        the header row (metadata the reader
                                        must ignore).
"""
import xlsxwriter

NBSP = " "


def main_workbook():
    wb = xlsxwriter.Workbook("xlsxwriter_main.xlsx")
    ws = wb.add_worksheet("Networks")
    ws.write_row(0, 0, ["Account ID", "Region", "CIDR", "VPC ID", "Name"])

    ws.write_string(1, 0, "123456789012")
    ws.write_string(1, 1, "us-east-1")
    ws.write_string(1, 2, "10.0.0.0/16")
    ws.write_string(1, 3, "vpc-0123abcd0000")
    ws.write_string(1, 4, "prod-main")

    ws.write_number(2, 0, 12345678)  # NUMBER, lost leading zeros
    ws.write_string(2, 1, "us-west-2")
    ws.write_string(2, 2, "10.1.0.0/16")
    ws.write_string(2, 3, "vpc-0456def00000")
    ws.write_string(2, 4, "staging")

    ws.write_string(3, 0, "000123456789")  # TEXT, leading zeros kept
    ws.write_string(3, 1, "eu-west-1")
    ws.write_string(3, 2, "10.2.0.0/16")
    ws.write_string(3, 3, "vpc-0789ghi00000")
    ws.write_string(3, 4, "dev")

    ws.write_number(4, 0, 123456789012)  # NUMBER, 12-digit
    ws.write_string(4, 1, "ap-southeast-1")
    ws.write_string(4, 2, "10.3.0.0/16")
    ws.write_string(4, 3, "vpc-0abc12340000")
    ws.write_string(4, 4, "qa")

    wrap = wb.add_format({"text_wrap": True})
    ws.write_string(5, 0, "234567890123")
    ws.write_string(5, 1, "sa-east-1")
    ws.write_string(5, 2, "10.4.0.0/16\n10.5.0.0/16", wrap)  # 2 CIDRs, line break
    ws.write_string(5, 3, "vpc-0def56780000")
    ws.write_string(5, 4, "multi-cidr")

    ws.write_string(6, 0, "345678901234")
    ws.write_string(6, 1, "ca-central-1")
    ws.write_string(6, 2, "10.6.0.0/16")
    ws.write_string(6, 3, "vpc-0ghi90120000")
    ws.write_string(6, 4, f"Acme,{NBSP}Inc")  # comma + NBSP

    # row index 7 (spreadsheet row 8) is never written at all: XlsxWriter
    # omits an untouched row from the sheet XML entirely.
    ws.write_string(8, 0, "456789012345")  # spreadsheet row 9
    ws.write_string(8, 1, "af-south-1")
    ws.write_string(8, 2, "10.7.0.0/16")
    ws.write_string(8, 3, "vpc-0jkl34560000")
    ws.write_string(8, 4, "after-gap")

    ws2 = wb.add_worksheet("Accounts")
    ws2.write_row(0, 0, ["Account ID", "Account Name"])
    ws2.write_string(1, 0, "111111111111")
    ws2.write_string(1, 1, "Example Co")
    ws2.write_number(2, 0, 22222222)  # NUMBER, lost leading zeros
    ws2.write_string(2, 1, "Other Co")

    wb.close()


def formula_cached_workbook():
    wb = xlsxwriter.Workbook("xlsxwriter_formula_cached.xlsx")
    ws = wb.add_worksheet("Networks")
    ws.write_row(0, 0, ["Account ID", "Region", "CIDR", "VPC ID", "Name"])
    # A string-result formula with an explicit cached value -- the third
    # positional arg XlsxWriter accepts for write_formula.
    ws.write_formula(1, 0, '=TEXT(123456789012,"0")', None, "123456789012")
    ws.write_string(1, 1, "us-east-1")
    ws.write_string(1, 2, "10.0.0.0/16")
    ws.write_string(1, 3, "vpc-0123abcd0000")
    ws.write_string(1, 4, "prod-main")
    # A numeric-result formula with a cached value, in an extra column, to
    # prove numeric <f>+<v> cells (no t="str") are also passed through.
    ws.write_row(0, 5, ["block_count"])
    ws.write_formula(1, 5, "=1+1", None, 2)
    wb.close()


def empty_column_workbook():
    wb = xlsxwriter.Workbook("xlsxwriter_empty_column.xlsx")
    ws = wb.add_worksheet("Networks")
    # Column D (index 3) is left entirely empty: header + every data row.
    ws.write_row(0, 0, ["Account ID", "Region", "CIDR"])
    ws.write(0, 4, "VPC ID")
    ws.write(0, 5, "Name")
    ws.write_string(1, 0, "123456789012")
    ws.write_string(1, 1, "us-east-1")
    ws.write_string(1, 2, "10.0.0.0/16")
    ws.write_string(1, 4, "vpc-0123abcd0000")
    ws.write_string(1, 5, "prod-main")
    ws.write_string(2, 0, "234567890123")
    ws.write_string(2, 1, "us-west-2")
    ws.write_string(2, 2, "10.1.0.0/16")
    ws.write_string(2, 4, "vpc-0456def00000")
    ws.write_string(2, 5, "staging")
    wb.close()


def frozen_autofilter_workbook():
    wb = xlsxwriter.Workbook("xlsxwriter_frozen_autofilter.xlsx")
    ws = wb.add_worksheet("Networks")
    ws.write_row(0, 0, ["Account ID", "Region", "CIDR", "VPC ID", "Name"])
    ws.write_string(1, 0, "123456789012")
    ws.write_string(1, 1, "us-east-1")
    ws.write_string(1, 2, "10.0.0.0/16")
    ws.write_string(1, 3, "vpc-0123abcd0000")
    ws.write_string(1, 4, "prod-main")
    ws.write_string(2, 0, "234567890123")
    ws.write_string(2, 1, "us-west-2")
    ws.write_string(2, 2, "10.1.0.0/16")
    ws.write_string(2, 3, "vpc-0456def00000")
    ws.write_string(2, 4, "staging")
    ws.freeze_panes(1, 1)  # freeze header row + first column
    ws.autofilter(0, 0, 2, 4)  # autofilter over the whole used range
    wb.close()


def wide_column_workbook():
    # Column ZZ (0-based index 701) holds real data, to prove parseCellRef's
    # base-26 column decoding is correct well past the single/double-letter
    # range most fixtures exercise.
    wb = xlsxwriter.Workbook("xlsxwriter_wide_column.xlsx")
    ws = wb.add_worksheet("Networks")
    ws.write_row(0, 0, ["Account ID", "Region", "CIDR"])
    ws.write(0, 701, "Name")  # ZZ1
    ws.write_string(1, 0, "123456789012")
    ws.write_string(1, 1, "us-east-1")
    ws.write_string(1, 2, "10.0.0.0/16")
    ws.write_string(1, 701, "prod-main")  # ZZ2
    wb.close()


if __name__ == "__main__":
    main_workbook()
    formula_cached_workbook()
    empty_column_workbook()
    frozen_autofilter_workbook()
    wide_column_workbook()
    print("xlsxwriter version:", xlsxwriter.__version__)
