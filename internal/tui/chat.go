package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/makewand/makewand/internal/i18n"
	"github.com/makewand/makewand/internal/model"
	"github.com/mattn/go-runewidth"
)

// maxChatHistory is the maximum number of messages to send to the model.
const maxChatHistory = 20
const summaryKeepRecent = 19 // keep 19 + 1 summary system message
const summaryMaxLines = 10
const summaryPerLineMaxChars = 180
const chatContextTokenBudget = 6000
const chatMinRecentMessages = 4
const contextTruncatedNotice = "\n- ... truncated to fit context budget ..."
const maxSlashSuggestions = 12

type slashCommandSuggestion struct {
	Command     string
	Description string
}

var rootSlashCommandSuggestions = []slashCommandSuggestion{
	{Command: "/help", Description: "Show commands"},
	{Command: "/clear", Description: "Clear conversation"},
	{Command: "/compact", Description: "Compact chat history"},
	{Command: "/model", Description: "Switch model profile"},
	{Command: "/memory", Description: "Show compacted session memory"},
	{Command: "/status", Description: "Show session status"},
	{Command: "/cost", Description: "Show session cost"},
	{Command: "/approve", Description: "Approve the pending action"},
	{Command: "/deny", Description: "Deny the pending action"},
	{Command: "/resume", Description: "Restore the last saved session"},
	{Command: "/exit", Description: "Quit makewand"},
}

var modelSlashCommandSuggestions = []slashCommandSuggestion{
	{Command: "/model fast", Description: "Fastest/cheapest routing"},
	{Command: "/model balanced", Description: "Default mode"},
	{Command: "/model power", Description: "Highest quality routing"},
}

// ChatMessage represents a message in the chat panel.
type ChatMessage struct {
	Role     string // "user", "assistant", "system", "status"
	Content  string
	Provider string // which AI model responded
	Cost     float64
}

// ChatPanel is the main chat interaction panel.
type ChatPanel struct {
	messages  []ChatMessage
	viewport  viewport.Model
	textarea  textarea.Model
	width     int
	height    int
	ready     bool
	streaming bool
	streamBuf *strings.Builder

	slashSelection int
}

const (
	minPanelWidth     = 1
	minPanelHeight    = 1
	minViewportWidth  = 1
	minViewportHeight = 1
	minInputWidth     = 1
)

// NewChatPanel creates a new chat panel.
func NewChatPanel() ChatPanel {
	ta := textarea.New()
	ta.Placeholder = i18n.Msg().ChatPlaceholder
	ta.Focus()
	ta.CharLimit = 4096
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()

	return ChatPanel{
		textarea:       ta,
		messages:       []ChatMessage{},
		streamBuf:      &strings.Builder{},
		slashSelection: -1,
	}
}

// AddMessage adds a message to the chat.
func (c *ChatPanel) AddMessage(msg ChatMessage) {
	c.messages = append(c.messages, msg)
	c.updateViewport()
}

func (c *ChatPanel) ResetMessages(messages []ChatMessage) {
	c.messages = append([]ChatMessage(nil), messages...)
	c.streamBuf.Reset()
	c.streaming = false
	c.updateViewport()
}

func (c *ChatPanel) CompactHistory() bool {
	if len(c.messages) == 0 {
		return false
	}

	preserved := make([]ChatMessage, 0, len(c.messages))
	dialog := make([]model.Message, 0, len(c.messages))
	for _, msg := range c.messages {
		switch msg.Role {
		case "user", "assistant":
			dialog = append(dialog, model.Message{Role: msg.Role, Content: msg.Content})
		default:
			preserved = append(preserved, msg)
		}
	}

	if len(dialog) <= maxChatHistory {
		return false
	}

	compacted := summarizeHistoryWindow(dialog)
	next := append([]ChatMessage(nil), preserved...)
	for _, msg := range compacted {
		next = append(next, ChatMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	c.messages = next
	c.updateViewport()
	return true
}

func (c *ChatPanel) relayout() {
	if c.width < minPanelWidth || c.height < minPanelHeight {
		return
	}

	headerHeight := 1
	inputHeight := 5 + c.commandSuggestionHeight()
	vpWidth := maxInt(c.width-2, minViewportWidth)
	vpHeight := maxInt(c.height-headerHeight-inputHeight, minViewportHeight)
	inputWidth := maxInt(c.width-2, minInputWidth)

	if !c.ready {
		c.viewport = viewport.New(vpWidth, vpHeight)
		c.ready = true
	} else {
		c.viewport.Width = vpWidth
		c.viewport.Height = vpHeight
	}

	c.textarea.SetWidth(inputWidth)
	c.viewport.SetContent(c.renderMessages())
}

// SetStreaming sets whether the AI is currently streaming a response.
func (c *ChatPanel) SetStreaming(streaming bool) {
	c.streaming = streaming
	if streaming {
		c.streamBuf.Reset()
	}
}

// AppendStream appends text to the current streaming response.
func (c *ChatPanel) AppendStream(text string) {
	c.streamBuf.WriteString(text)
	c.updateViewport()
}

// FinishStream finishes the current stream and adds it as a message.
func (c *ChatPanel) FinishStream(provider string, cost float64) {
	if c.streamBuf.Len() > 0 {
		c.messages = append(c.messages, ChatMessage{
			Role:     "assistant",
			Content:  c.streamBuf.String(),
			Provider: provider,
			Cost:     cost,
		})
		c.streamBuf.Reset()
	}
	c.streaming = false
	c.updateViewport()
}

// ToModelMessages converts chat messages to model API messages.
// Uses a summary+window strategy:
//   - Keep recent messages verbatim.
//   - Compress older messages into one system summary entry.
func (c *ChatPanel) ToModelMessages() []model.Message {
	var msgs []model.Message
	for _, m := range c.messages {
		if m.Role == "user" || m.Role == "assistant" {
			msgs = append(msgs, model.Message{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	if len(msgs) == 0 {
		return nil
	}

	if len(msgs) > maxChatHistory {
		msgs = summarizeHistoryWindow(msgs)
	}

	msgs = trimToTokenBudget(msgs, chatContextTokenBudget)
	return msgs
}

func summarizeHistoryWindow(msgs []model.Message) []model.Message {
	if len(msgs) <= maxChatHistory {
		return msgs
	}

	keep := summaryKeepRecent
	if keep >= maxChatHistory {
		keep = maxChatHistory - 1
	}
	if keep < 1 {
		keep = 1
	}
	if keep > len(msgs) {
		keep = len(msgs)
	}

	older := msgs[:len(msgs)-keep]
	recent := msgs[len(msgs)-keep:]
	summary := model.Message{
		Role:    "system",
		Content: buildHistorySummary(older),
	}
	out := make([]model.Message, 0, 1+len(recent))
	out = append(out, summary)
	out = append(out, recent...)
	return out
}

func trimToTokenBudget(msgs []model.Message, budget int) []model.Message {
	if budget <= 0 || len(msgs) == 0 {
		return msgs
	}

	out := append([]model.Message(nil), msgs...)
	for estimateModelMessagesTokens(out) > budget {
		dropIdx := 0
		minLen := chatMinRecentMessages
		if out[0].Role == "system" {
			dropIdx = 1
			minLen = chatMinRecentMessages + 1 // summary + at least recent turns
		}
		if len(out) <= minLen || dropIdx >= len(out)-1 {
			break
		}
		out = append(out[:dropIdx], out[dropIdx+1:]...)
	}

	// If the context is still too large, truncate only the summary/oldest message.
	if estimateModelMessagesTokens(out) > budget {
		for pass := 0; pass < 8 && estimateModelMessagesTokens(out) > budget; pass++ {
			changed := false
			// Prefer shrinking older context first.
			for i := 0; i < len(out)-1 && estimateModelMessagesTokens(out) > budget; i++ {
				overflow := estimateModelMessagesTokens(out) - budget
				if shrinkMessageForBudget(&out[i], overflow, 96) {
					changed = true
				}
			}
			// As a last resort, shrink the newest message too.
			if estimateModelMessagesTokens(out) > budget {
				overflow := estimateModelMessagesTokens(out) - budget
				if shrinkMessageForBudget(&out[len(out)-1], overflow, 128) {
					changed = true
				}
			}
			if !changed {
				break
			}
		}
	}

	return out
}

func shrinkMessageForBudget(msg *model.Message, overflowTokens, minRunes int) bool {
	base := strings.TrimSuffix(msg.Content, contextTruncatedNotice)
	runes := []rune(base)
	if len(runes) <= minRunes {
		return false
	}

	cut := overflowTokens*4 + 32
	maxCut := len(runes) - minRunes
	if cut > maxCut {
		cut = maxCut
	}
	if cut <= 0 {
		return false
	}
	keep := len(runes) - cut
	if keep < minRunes {
		keep = minRunes
	}
	if keep >= len(runes) {
		return false
	}
	msg.Content = string(runes[:keep]) + contextTruncatedNotice
	return true
}

func estimateModelMessagesTokens(msgs []model.Message) int {
	total := 2 // assistant priming tokens
	for _, m := range msgs {
		total += 4 // per-message framing overhead
		total += estimateTextTokens(m.Content)
	}
	return total
}

func estimateTextTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes <= 0 {
		return 0
	}
	return (runes + 3) / 4
}

func buildHistorySummary(msgs []model.Message) string {
	var b strings.Builder
	b.WriteString("Conversation summary of earlier context:\n")

	render := func(m model.Message) string {
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		content := strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " "))
		content = strings.Join(strings.Fields(content), " ")
		if content == "" {
			content = "(empty)"
		}
		if utf8.RuneCountInString(content) > summaryPerLineMaxChars {
			content = truncateRunes(content, summaryPerLineMaxChars) + "..."
		}
		return fmt.Sprintf("- %s: %s", role, content)
	}

	if len(msgs) <= summaryMaxLines {
		for _, m := range msgs {
			b.WriteString(render(m))
			b.WriteString("\n")
		}
		return b.String()
	}

	headCount := summaryMaxLines / 2
	tailCount := summaryMaxLines - headCount
	for _, m := range msgs[:headCount] {
		b.WriteString(render(m))
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("- ... %d earlier turns omitted ...\n", len(msgs)-summaryMaxLines))
	for _, m := range msgs[len(msgs)-tailCount:] {
		b.WriteString(render(m))
		b.WriteString("\n")
	}
	return b.String()
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes])
}

func (c *ChatPanel) updateViewport() {
	if !c.ready {
		return
	}
	if c.viewport.Width < minViewportWidth || c.viewport.Height < minViewportHeight {
		return
	}
	c.viewport.SetContent(c.renderMessages())
	c.viewport.GotoBottom()
}

func (c *ChatPanel) renderMessages() string {
	var b strings.Builder
	maxWidth := c.width - 4

	for _, msg := range c.messages {
		switch msg.Role {
		case "user":
			b.WriteString(userMsgStyle.Render("You") + "\n")
			b.WriteString(wrapText(msg.Content, maxWidth) + "\n\n")
		case "assistant":
			label := "AI"
			if msg.Provider != "" {
				label = fmt.Sprintf("AI (%s)", msg.Provider)
			}
			b.WriteString(aiMsgStyle.Render(label) + "\n")
			b.WriteString(wrapText(msg.Content, maxWidth) + "\n")
			if msg.Cost > 0 {
				b.WriteString(mutedStyle.Render(fmt.Sprintf("  $%.4f", msg.Cost)) + "\n")
			}
			b.WriteString("\n")
		case "system":
			b.WriteString(mutedStyle.Render("--- "+msg.Content+" ---") + "\n\n")
		case "status":
			b.WriteString(warningStyle.Render("* "+msg.Content) + "\n\n")
		}
	}

	// Show streaming content
	streamStr := c.streamBuf.String()
	if c.streaming && streamStr != "" {
		b.WriteString(aiMsgStyle.Render("AI") + " " + spinnerStyle.Render("*") + "\n")
		b.WriteString(wrapText(streamStr, maxWidth) + "\n")
	} else if c.streaming {
		b.WriteString(spinnerStyle.Render("* "+i18n.Msg().ChatThinkingAnim) + "\n")
	}

	return b.String()
}

// Init implements tea.Model.
func (c ChatPanel) Init() tea.Cmd {
	return textarea.Blink
}

// Update implements tea.Model.
func (c ChatPanel) Update(msg tea.Msg) (ChatPanel, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width = maxInt(msg.Width, minPanelWidth)
		c.height = maxInt(msg.Height, minPanelHeight)

	case tea.KeyMsg:
		if msg.Type == tea.KeyTab && c.applySlashCompletion() {
			c.relayout()
			c.updateViewport()
			return c, nil
		}
		if (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) && c.moveSlashSelection(keySelectionDelta(msg.Type)) {
			c.relayout()
			c.updateViewport()
			return c, nil
		}
	}

	var vpCmd tea.Cmd
	c.viewport, vpCmd = c.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	var taCmd tea.Cmd
	c.textarea, taCmd = c.textarea.Update(msg)
	cmds = append(cmds, taCmd)

	c.syncSlashSelection()
	c.relayout()
	c.updateViewport()

	return c, tea.Batch(cmds...)
}

// View implements tea.Model.
func (c ChatPanel) View() string {
	if !c.ready {
		return "Loading..."
	}

	inputArea := c.textarea.View()
	if suggestions := c.renderSlashSuggestions(); suggestions != "" {
		inputArea = suggestions + "\n" + inputArea
	}

	return fmt.Sprintf(
		"%s\n%s",
		c.viewport.View(),
		inputArea,
	)
}

// InputValue returns the current text input value.
func (c *ChatPanel) InputValue() string {
	return c.textarea.Value()
}

// ClearInput clears the text input.
func (c *ChatPanel) ClearInput() {
	c.textarea.Reset()
	c.slashSelection = -1
	c.relayout()
}

// Focus focuses the text input.
func (c *ChatPanel) Focus() {
	c.textarea.Focus()
}

func (c *ChatPanel) applySlashCompletion() bool {
	input := strings.TrimSpace(c.textarea.Value())
	if input == "" || !strings.HasPrefix(input, "/") {
		return false
	}

	suggestions := c.currentSlashSuggestions()
	if len(suggestions) == 0 {
		return false
	}

	target := longestCommonSlashPrefix(suggestions)
	if len(suggestions) == 1 || target == input {
		index := c.slashSelection
		if index < 0 || index >= len(suggestions) {
			index = 0
		}
		target = suggestions[index].Command
	}
	if target == "" || target == input {
		return false
	}

	c.textarea.SetValue(target)
	c.textarea.SetCursor(len(target))
	c.syncSlashSelection()
	return true
}

func (c *ChatPanel) ApplySelectedSlashSuggestion() bool {
	suggestions := c.visibleSlashSuggestions()
	if len(suggestions) == 0 {
		return false
	}

	index := c.slashSelection
	if index < 0 || index >= len(suggestions) {
		index = 0
	}
	target := suggestions[index].Command
	if strings.TrimSpace(c.textarea.Value()) == target {
		return false
	}

	c.textarea.SetValue(target)
	c.textarea.SetCursor(len(target))
	c.syncSlashSelection()
	c.relayout()
	return true
}

func (c ChatPanel) HasSlashSuggestions() bool {
	return len(c.visibleSlashSuggestions()) > 0
}

// LastAssistantContent returns the content of the most recent assistant message.
func (c *ChatPanel) LastAssistantContent() string {
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == "assistant" {
			return c.messages[i].Content
		}
	}
	return ""
}

// UpdatePlaceholder refreshes the textarea placeholder from i18n.
func (c *ChatPanel) UpdatePlaceholder() {
	c.textarea.Placeholder = i18n.Msg().ChatPlaceholder
}

func (c ChatPanel) currentSlashSuggestions() []slashCommandSuggestion {
	input := strings.ToLower(strings.TrimSpace(c.textarea.Value()))
	if input == "" || !strings.HasPrefix(input, "/") || strings.Contains(input, "\n") {
		return nil
	}

	base := rootSlashCommandSuggestions
	if input == "/model" || strings.HasPrefix(input, "/model ") {
		base = modelSlashCommandSuggestions
	}

	matches := make([]slashCommandSuggestion, 0, len(base))
	for _, suggestion := range base {
		if strings.HasPrefix(strings.ToLower(suggestion.Command), input) {
			matches = append(matches, suggestion)
		}
	}
	return matches
}

func (c ChatPanel) visibleSlashSuggestions() []slashCommandSuggestion {
	suggestions := c.currentSlashSuggestions()
	if len(suggestions) > maxSlashSuggestions {
		suggestions = suggestions[:maxSlashSuggestions]
	}
	return suggestions
}

func (c *ChatPanel) syncSlashSelection() {
	suggestions := c.visibleSlashSuggestions()
	if len(suggestions) == 0 {
		c.slashSelection = -1
		return
	}
	if c.slashSelection < 0 || c.slashSelection >= len(suggestions) {
		c.slashSelection = 0
	}
}

func (c *ChatPanel) moveSlashSelection(delta int) bool {
	suggestions := c.visibleSlashSuggestions()
	if len(suggestions) == 0 {
		return false
	}

	c.syncSlashSelection()
	c.slashSelection += delta
	if c.slashSelection < 0 {
		c.slashSelection = len(suggestions) - 1
	} else if c.slashSelection >= len(suggestions) {
		c.slashSelection = 0
	}
	return true
}

func (c ChatPanel) commandSuggestionHeight() int {
	suggestions := c.visibleSlashSuggestions()
	if len(suggestions) == 0 {
		return 0
	}
	return 1 + len(suggestions)
}

func (c ChatPanel) renderSlashSuggestions() string {
	suggestions := c.visibleSlashSuggestions()
	if len(suggestions) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(helpStyle.Render("Commands (Up/Down to select, Enter/Tab to apply)") + "\n")
	selected := c.slashSelection
	if selected < 0 || selected >= len(suggestions) {
		selected = 0
	}
	for i, suggestion := range suggestions {
		prefix := "  "
		commandStyle := lipgloss.NewStyle()
		if i == selected {
			prefix = selectedStyle.Render("> ")
			commandStyle = selectedStyle
		}
		b.WriteString(prefix)
		b.WriteString(commandStyle.Render(suggestion.Command))
		b.WriteString("  ")
		b.WriteString(mutedStyle.Render(suggestion.Description))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func longestCommonSlashPrefix(suggestions []slashCommandSuggestion) string {
	if len(suggestions) == 0 {
		return ""
	}

	prefix := suggestions[0].Command
	for _, suggestion := range suggestions[1:] {
		for !strings.HasPrefix(suggestion.Command, prefix) && prefix != "" {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

func keySelectionDelta(key tea.KeyType) int {
	if key == tea.KeyUp {
		return -1
	}
	return 1
}

func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if runewidth.StringWidth(line) <= width {
			result.WriteString(line + "\n")
			continue
		}
		words := strings.Fields(line)
		current := ""
		currentW := 0
		for _, word := range words {
			wordW := runewidth.StringWidth(word)
			if current == "" {
				current = word
				currentW = wordW
			} else if currentW+1+wordW <= width {
				current += " " + word
				currentW += 1 + wordW
			} else {
				result.WriteString(current + "\n")
				current = word
				currentW = wordW
			}
		}
		if current != "" {
			result.WriteString(current + "\n")
		}
	}
	return strings.TrimRight(result.String(), "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
