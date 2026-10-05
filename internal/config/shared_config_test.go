package config

import (
	"bytes"
	"encoding/json"
	"os"
	"runtime"
	"testing"
)

func readSharedConfig(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSavePreservesPythonFieldsAndLatestDiskUpdates(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	initial := `{"language":"en","enabled_providers":{"local":false,"claude":true},"local_model":"qwen-test","api":{"claude":{"model":"test-model"}},"future_config":{"counter":9007199254740993}}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	// Model a Python frontend update after the Go frontend loaded its config.
	disk := readSharedConfig(t, path)
	disk["local_model"] = json.RawMessage(`"changed-by-python"`)
	disk["enabled_providers"] = json.RawMessage(`{"local":true,"claude":false}`)
	disk["active_providers"] = json.RawMessage(`["codex"]`)
	disk["local_model_enabled"] = json.RawMessage(`true`)
	disk["python_only_new_field"] = json.RawMessage(`{"enabled":true}`)
	updated, _ := json.Marshal(disk)
	if err := os.WriteFile(path, updated, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Language = "zh"
	cfg.ApprovalMode = ApprovalModeSafe
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved := readSharedConfig(t, path)
	for _, key := range []string{"enabled_providers", "active_providers", "local_model_enabled", "local_model", "api", "future_config", "python_only_new_field"} {
		var want, got bytes.Buffer
		if err := json.Compact(&want, disk[key]); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&got, saved[key]); err != nil {
			t.Fatal(err)
		}
		if want.String() != got.String() {
			t.Fatalf("Python field %s changed: got=%s want=%s", key, got.String(), want.String())
		}
	}
	if string(saved["language"]) != `"zh"` || string(saved["approval_mode"]) != `"safe"` {
		t.Fatalf("Go updates missing: %s", saved)
	}
	// A subsequent Go round trip must preserve the same cross-language fields.
	cfg, err = LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	// Unix mode bits are a POSIX contract. Windows private ACLs are verified
	// separately on the native owned QA root; retain the file/type check here.
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("private config permissions: %v %v", info, err)
	}
}

func TestSaveClearsKnownFieldsWithoutResurrectingEnvironmentKeys(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "env-claude")
	t.Setenv("GEMINI_API_KEY", "env-gemini")
	t.Setenv("OPENAI_API_KEY", "env-openai")
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	existing := `{"CLAUDE_API_KEY":"disk-claude","gemini_api_key":"disk-gemini","OpenAI_API_KEY":"disk-openai","custom_providers":[{"name":"old","command":"old"}],"claude_model":"old-model","enabled_providers":{"local":false}}`
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{SkipCLIDetection: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClaudeModel = ""
	cfg.CustomProviders = nil
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved := readSharedConfig(t, path)
	for _, key := range []string{"CLAUDE_API_KEY", "claude_api_key", "gemini_api_key", "OpenAI_API_KEY", "openai_api_key", "custom_providers", "claude_model"} {
		if _, exists := saved[key]; exists {
			t.Fatalf("removed Go field %s was resurrected", key)
		}
	}
	if len(saved["enabled_providers"]) == 0 {
		t.Fatal("Python enablement lost")
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("env-")) || bytes.Contains(data, []byte("disk-")) {
		t.Fatal("API credential persisted")
	}
}

func TestSaveRejectsMalformedExistingSharedConfigWithoutClobbering(t *testing.T) {
	t.Setenv("MAKEWAND_CONFIG_DIR", t.TempDir())
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"enabled_providers":`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Save(DefaultConfig()); err == nil {
		t.Fatal("malformed shared config was overwritten")
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, original) {
		t.Fatal("failed save changed config")
	}
}
