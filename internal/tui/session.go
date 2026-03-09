package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/model"
)

const (
	chatSessionVersion      = 1
	chatSessionDirName      = "sessions"
	restoredSessionPrefix   = "Restored previous session"
	noCompactedMemoryNotice = "No compacted memory yet. Conversation is still within the active context window."
)

type chatSessionState struct {
	Version     int                  `json:"version"`
	ProjectPath string               `json:"project_path"`
	SavedAt     string               `json:"saved_at"`
	UsageMode   string               `json:"usage_mode,omitempty"`
	Messages    []ChatMessage        `json:"messages,omitempty"`
	Costs       []persistedCostEntry `json:"costs,omitempty"`
}

type persistedCostEntry struct {
	Provider       string  `json:"provider"`
	Cost           float64 `json:"cost"`
	IsSubscription bool    `json:"is_subscription,omitempty"`
	InputTokens    int     `json:"input_tokens,omitempty"`
	OutputTokens   int     `json:"output_tokens,omitempty"`
}

func (a *App) saveChatSession() error {
	if a.mode != ModeChat {
		return nil
	}

	projectPath := a.sessionProjectPath()
	if projectPath == "" {
		return nil
	}

	sessionFile, err := chatSessionFilePath(projectPath)
	if err != nil {
		return err
	}

	messages := filterSessionMessages(a.chat.messages)
	hasConversation := false
	for _, msg := range messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			hasConversation = true
			break
		}
	}
	if !hasConversation {
		a.sessionFile = sessionFile
		a.lastSessionSavedAt = ""
		if err := os.Remove(sessionFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	state := chatSessionState{
		Version:     chatSessionVersion,
		ProjectPath: projectPath,
		SavedAt:     time.Now().UTC().Format(time.RFC3339),
		Messages:    messages,
		Costs:       snapshotPersistedCosts(a.cost.Snapshot()),
	}
	if a.router.ModeSet() {
		state.UsageMode = a.router.Mode().String()
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmp := sessionFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, sessionFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	a.sessionFile = sessionFile
	a.lastSessionSavedAt = state.SavedAt
	return nil
}

func (a *App) restoreChatSession() (bool, error) {
	if a.mode != ModeChat {
		return false, nil
	}

	projectPath := a.sessionProjectPath()
	if projectPath == "" {
		return false, nil
	}

	sessionFile, err := chatSessionFilePath(projectPath)
	if err != nil {
		return false, err
	}
	a.sessionFile = sessionFile

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var state chatSessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, err
	}
	if len(state.Messages) == 0 {
		return false, nil
	}

	a.chat.ResetMessages(append([]ChatMessage(nil), state.Messages...))
	if mode, ok := parseSavedUsageMode(state.UsageMode); ok {
		a.router.SetMode(mode)
	}
	a.cost.Restore(restorePersistedCosts(state.Costs))
	a.lastSessionSavedAt = state.SavedAt
	a.restoredSession = true
	a.restoredMessageCount = len(state.Messages)
	a.chat.AddMessage(ChatMessage{
		Role:    "status",
		Content: fmt.Sprintf("%s (%d messages). Use /clear to start fresh.", restoredSessionPrefix, len(state.Messages)),
	})
	return true, nil
}

func (a App) sessionProjectPath() string {
	if a.project != nil && strings.TrimSpace(a.project.Path) != "" {
		return a.project.Path
	}
	return ""
}

func chatSessionFilePath(projectPath string) (string, error) {
	absPath, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}

	cfgDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	sessionsDir := filepath.Join(cfgDir, chatSessionDirName)
	if err := os.MkdirAll(sessionsDir, 0700); err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(absPath))
	base := sanitizeSessionStem(filepath.Base(absPath))
	if base == "" {
		base = "workspace"
	}
	return filepath.Join(sessionsDir, fmt.Sprintf("%s-%s.json", base, hex.EncodeToString(sum[:6]))), nil
}

func sanitizeSessionStem(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func filterSessionMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for _, msg := range messages {
		if strings.HasPrefix(strings.TrimSpace(msg.Content), restoredSessionPrefix) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func snapshotPersistedCosts(entries []costEntry) []persistedCostEntry {
	out := make([]persistedCostEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, persistedCostEntry{
			Provider:       entry.provider,
			Cost:           entry.cost,
			IsSubscription: entry.isSubscription,
			InputTokens:    entry.inputTokens,
			OutputTokens:   entry.outputTokens,
		})
	}
	return out
}

func restorePersistedCosts(entries []persistedCostEntry) []costEntry {
	out := make([]costEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, costEntry{
			provider:       entry.Provider,
			cost:           entry.Cost,
			isSubscription: entry.IsSubscription,
			inputTokens:    entry.InputTokens,
			outputTokens:   entry.OutputTokens,
		})
	}
	return out
}

func parseSavedUsageMode(value string) (model.UsageMode, bool) {
	return model.ParseUsageMode(value)
}

func (a App) memorySummary() string {
	modelMessages := a.chat.ToModelMessages()
	for _, msg := range modelMessages {
		if msg.Role == "system" && strings.HasPrefix(msg.Content, "Conversation summary of earlier context:") {
			return strings.TrimSpace(msg.Content)
		}
	}

	lines := []string{noCompactedMemoryNotice}
	if a.restoredSession {
		lines = append(lines, fmt.Sprintf("%s at %s.", restoredSessionPrefix, sessionTimeLabel(a.lastSessionSavedAt)))
	}
	if a.sessionFile != "" {
		lines = append(lines, fmt.Sprintf("Session file: %s", a.sessionFile))
	}
	return strings.Join(lines, "\n")
}

func sessionTimeLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown time"
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts.Local().Format("2006-01-02 15:04:05")
	}
	return value
}
