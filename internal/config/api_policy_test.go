package config

import (
	"os"
	"testing"
)

func TestAPIPolicyDefaultsAndExplicitConfiguration(t *testing.T) {
	previous, existed := os.LookupEnv("MAKEWAND_API_POLICY")
	if err := os.Unsetenv("MAKEWAND_API_POLICY"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("MAKEWAND_API_POLICY", previous)
		} else {
			_ = os.Unsetenv("MAKEWAND_API_POLICY")
		}
	})
	cfg := DefaultConfig()
	cfg.OpenAIAPIKey = "key-does-not-authorize-spend"
	if cfg.PaidAPIAllowed() || cfg.HasAnyModel() {
		t.Fatal("key implicitly authorized direct cloud API")
	}
	cfg.APIPolicy = APIPolicyAllowPaid
	if !cfg.PaidAPIAllowed() || !cfg.HasAnyModel() {
		t.Fatal("explicit paid policy not honored")
	}
	t.Setenv("MAKEWAND_API_POLICY", "invalid")
	if cfg.PaidAPIAllowed() {
		t.Fatal("unknown override must fail closed")
	}
}

func TestAPIPolicyRoundTrip(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	cfg := DefaultConfig()
	cfg.APIPolicy = APIPolicyAllowPaid
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIPolicy != APIPolicyAllowPaid {
		t.Fatalf("api policy lost: %q", loaded.APIPolicy)
	}
}
