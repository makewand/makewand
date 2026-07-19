package main

import (
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

func TestResolveRepoTrust(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    model.RepoTrust
		wantErr bool
	}{
		{name: "empty defaults to trusted", value: "", want: model.RepoTrustTrusted},
		{name: "trusted", value: "trusted", want: model.RepoTrustTrusted},
		{name: "untrusted", value: "untrusted", want: model.RepoTrustUntrusted},
		{name: "case-insensitive untrusted", value: "UNTRUSTED", want: model.RepoTrustUntrusted},
		{name: "trimmed", value: "  untrusted  ", want: model.RepoTrustUntrusted},
		{name: "invalid value errors", value: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRepoTrust(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveRepoTrust(%q) error = nil, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRepoTrust(%q) unexpected error = %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("resolveRepoTrust(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestRepoTrustFlagThreadsToRouter verifies that the resolved --repo-trust value
// applied the way the headless path applies it (model.NewRouter + SetRepoTrust)
// lands on the constructed router.
func TestRepoTrustFlagThreadsToRouter(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig()

	t.Run("untrusted flag", func(t *testing.T) {
		trust, err := resolveRepoTrust("untrusted")
		if err != nil {
			t.Fatalf("resolveRepoTrust: %v", err)
		}
		r, err := model.NewRouter(cfg)
		if err != nil {
			t.Fatalf("NewRouter: %v", err)
		}
		r.SetRepoTrust(trust)
		if r.RepoTrust() != model.RepoTrustUntrusted {
			t.Fatalf("router.RepoTrust() = %v, want %v", r.RepoTrust(), model.RepoTrustUntrusted)
		}
	})

	t.Run("default flag stays trusted", func(t *testing.T) {
		trust, err := resolveRepoTrust("trusted")
		if err != nil {
			t.Fatalf("resolveRepoTrust: %v", err)
		}
		r, err := model.NewRouter(cfg)
		if err != nil {
			t.Fatalf("NewRouter: %v", err)
		}
		r.SetRepoTrust(trust)
		if r.RepoTrust() != model.RepoTrustTrusted {
			t.Fatalf("router.RepoTrust() = %v, want %v", r.RepoTrust(), model.RepoTrustTrusted)
		}
	})
}
