package tui

import (
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

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

func TestStartPromptMsg_SubmitsModeCommand(t *testing.T) {
	cfg := config.DefaultConfig()
	app := *NewApp(ModeChat, cfg, "")
	app.router.SetMode(model.ModeBalanced)

	nextModel, _ := app.Update(startPromptMsg{input: "/mode power"})
	next, ok := nextModel.(App)
	if !ok {
		t.Fatalf("Update() returned unexpected model type: %T", nextModel)
	}
	if got := next.router.Mode(); got != model.ModePower {
		t.Fatalf("router mode = %v, want %v", got, model.ModePower)
	}
}
