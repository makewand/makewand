// Package csvsafe keeps spreadsheet applications from evaluating exported CSV
// cells as formulas (CSV injection).
package csvsafe

import (
	"strings"
	"unicode/utf8"
)

// formulaPrefixes are the first characters that make Excel, LibreOffice or
// Google Sheets evaluate a cell, or strip whitespace and then evaluate it,
// including the full-width forms ＝ ＋ － ＠, which some read as = + - @
// (OWASP, CSV injection).
const formulaPrefixes = "=+-@\t\r\n＝＋－＠"

// Cell returns v as a cell a spreadsheet shows as text: a value whose first
// character is =, +, -, @, a tab, a carriage return, a line feed or one of
// the full-width ＝ ＋ － ＠ gets a leading single quote. A plain ASCII decimal
// number such as -5 or 0.25 is not a formula and is kept.
func Cell(v string) string {
	first, _ := utf8.DecodeRuneInString(v)
	if v == "" || !strings.ContainsRune(formulaPrefixes, first) || plainNumber(v) {
		return v
	}
	return "'" + v
}

// Record returns a new record with every cell made safe by Cell.
func Record(cells []string) []string {
	out := make([]string, len(cells))
	for i, v := range cells {
		out[i] = Cell(v)
	}
	return out
}

// plainNumber reports whether v is an optionally signed ASCII decimal number.
func plainNumber(v string) bool {
	if v[0] == '-' || v[0] == '+' {
		v = v[1:]
	}
	whole, fraction, dot := strings.Cut(v, ".")
	return digits(whole) && (!dot || digits(fraction))
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
