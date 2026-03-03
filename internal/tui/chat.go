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
		textarea:  ta,
		messages:  []ChatMessage{},
		streamBuf: &strings.Builder{},
	}
}

// AddMessage adds a message to the chat.
func (c *ChatPanel) AddMessage(msg ChatMessage) {
	c.messages = append(c.messages, msg)
	c.updateViewport()
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

		headerHeight := 1
		inputHeight := 5
		vpWidth := maxInt(c.width-2, minViewportWidth)
		vpHeight := maxInt(c.height-headerHeight-inputHeight, minViewportHeight)
		inputWidth := maxInt(c.width-2, minInputWidth)

		if !c.ready {
			c.viewport = viewport.New(vpWidth, vpHeight)
			c.viewport.SetContent(c.renderMessages())
			c.ready = true
		} else {
			c.viewport.Width = vpWidth
			c.viewport.Height = vpHeight
		}

		c.textarea.SetWidth(inputWidth)
	}

	var vpCmd tea.Cmd
	c.viewport, vpCmd = c.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	var taCmd tea.Cmd
	c.textarea, taCmd = c.textarea.Update(msg)
	cmds = append(cmds, taCmd)

	return c, tea.Batch(cmds...)
}

// View implements tea.Model.
func (c ChatPanel) View() string {
	if !c.ready {
		return "Loading..."
	}

	return fmt.Sprintf(
		"%s\n%s",
		c.viewport.View(),
		c.textarea.View(),
	)
}

// InputValue returns the current text input value.
func (c *ChatPanel) InputValue() string {
	return c.textarea.Value()
}

// ClearInput clears the text input.
func (c *ChatPanel) ClearInput() {
	c.textarea.Reset()
}

// Focus focuses the text input.
func (c *ChatPanel) Focus() {
	c.textarea.Focus()
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
