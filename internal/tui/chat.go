package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/makewand/makewand/internal/model"
)

// ChatMessage represents a message in the chat panel.
type ChatMessage struct {
	Role     string // "user", "assistant", "system", "status"
	Content  string
	Provider string // which AI model responded
	Cost     float64
}

// ChatPanel is the main chat interaction panel.
type ChatPanel struct {
	messages []ChatMessage
	viewport viewport.Model
	textarea textarea.Model
	width    int
	height   int
	ready    bool
	streaming bool
	streamBuf string
}

// NewChatPanel creates a new chat panel.
func NewChatPanel() ChatPanel {
	ta := textarea.New()
	ta.Placeholder = "Type your message... (Enter to send, Ctrl+D for multiline)"
	ta.Focus()
	ta.CharLimit = 4096
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()

	return ChatPanel{
		textarea: ta,
		messages: []ChatMessage{},
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
		c.streamBuf = ""
	}
}

// AppendStream appends text to the current streaming response.
func (c *ChatPanel) AppendStream(text string) {
	c.streamBuf += text
	c.updateViewport()
}

// FinishStream finishes the current stream and adds it as a message.
func (c *ChatPanel) FinishStream(provider string, cost float64) {
	if c.streamBuf != "" {
		c.messages = append(c.messages, ChatMessage{
			Role:     "assistant",
			Content:  c.streamBuf,
			Provider: provider,
			Cost:     cost,
		})
		c.streamBuf = ""
	}
	c.streaming = false
	c.updateViewport()
}

// ToModelMessages converts chat messages to model API messages.
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
	return msgs
}

func (c *ChatPanel) updateViewport() {
	if !c.ready {
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
				b.WriteString(mutedStyle.Render(fmt.Sprintf("  💰 $%.4f", msg.Cost)) + "\n")
			}
			b.WriteString("\n")
		case "system":
			b.WriteString(mutedStyle.Render("─── "+msg.Content+" ───") + "\n\n")
		case "status":
			b.WriteString(warningStyle.Render("⚡ "+msg.Content) + "\n\n")
		}
	}

	// Show streaming content
	if c.streaming && c.streamBuf != "" {
		b.WriteString(aiMsgStyle.Render("AI") + " " + spinnerStyle.Render("●") + "\n")
		b.WriteString(wrapText(c.streamBuf, maxWidth) + "\n")
	} else if c.streaming {
		b.WriteString(spinnerStyle.Render("● Thinking...") + "\n")
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
		c.width = msg.Width
		c.height = msg.Height

		headerHeight := 1
		inputHeight := 5
		vpHeight := c.height - headerHeight - inputHeight

		if !c.ready {
			c.viewport = viewport.New(c.width-2, vpHeight)
			c.viewport.SetContent(c.renderMessages())
			c.ready = true
		} else {
			c.viewport.Width = c.width - 2
			c.viewport.Height = vpHeight
		}

		c.textarea.SetWidth(c.width - 2)
	}

	// Update viewport
	var vpCmd tea.Cmd
	c.viewport, vpCmd = c.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	// Update textarea
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

func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if len(line) <= width {
			result.WriteString(line + "\n")
			continue
		}
		// Simple word wrap
		words := strings.Fields(line)
		current := ""
		for _, word := range words {
			if current == "" {
				current = word
			} else if len(current)+1+len(word) <= width {
				current += " " + word
			} else {
				result.WriteString(current + "\n")
				current = word
			}
		}
		if current != "" {
			result.WriteString(current + "\n")
		}
	}
	return strings.TrimRight(result.String(), "\n")
}
