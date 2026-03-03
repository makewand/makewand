package tui

import "testing"

func TestIsLGTMResponse(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		isLGTM bool
	}{
		{name: "exact", input: "LGTM", isLGTM: true},
		{name: "case and whitespace", input: "  lgtm  ", isLGTM: true},
		{name: "negative phrase", input: "not LGTM", isLGTM: false},
		{name: "extra text", input: "LGTM with minor style nits", isLGTM: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLGTMResponse(tt.input); got != tt.isLGTM {
				t.Fatalf("isLGTMResponse(%q) = %v, want %v", tt.input, got, tt.isLGTM)
			}
		})
	}
}
