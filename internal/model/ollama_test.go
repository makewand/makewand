package model

import "testing"

func TestValidateOllamaURL_LocalhostAllowed(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "")
	if err := validateOllamaURL("http://localhost:11434"); err != nil {
		t.Fatalf("validateOllamaURL(localhost) error = %v, want nil", err)
	}
}

func TestValidateOllamaURL_DefaultBlocksRemote(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "")
	if err := validateOllamaURL("http://10.0.0.2:11434"); err == nil {
		t.Fatal("validateOllamaURL(remote) error = nil, want remote host rejection")
	}
}

func TestValidateOllamaURL_AllowRemoteOverride(t *testing.T) {
	t.Setenv("MAKEWAND_OLLAMA_ALLOW_REMOTE", "1")
	if err := validateOllamaURL("http://10.0.0.2:11434"); err != nil {
		t.Fatalf("validateOllamaURL(remote with override) error = %v, want nil", err)
	}
}
