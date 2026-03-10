package tui

import "github.com/charmbracelet/lipgloss"

// Color palette
var (
	colorPrimary   = lipgloss.Color("#7C3AED") // purple
	colorSecondary = lipgloss.Color("#06B6D4") // cyan
	colorSuccess   = lipgloss.Color("#10B981") // green
	colorWarning   = lipgloss.Color("#F59E0B") // amber
	colorError     = lipgloss.Color("#EF4444") // red
	colorMuted     = lipgloss.Color("#6B7280") // gray
	colorText      = lipgloss.Color("#E5E7EB") // light gray
	colorBg        = lipgloss.Color("#1F2937") // dark gray
	colorBorder    = lipgloss.Color("#374151") // border gray
)

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true).
			Padding(0, 1)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(colorSecondary).
			Italic(true)

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	chatBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorPrimary).
			Padding(0, 1)

	fileBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSecondary).
			Padding(0, 1)

	statusBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorMuted).
				Padding(0, 1)

	userMsgStyle = lipgloss.NewStyle().
			Foreground(colorSecondary).
			Bold(true)

	aiMsgStyle = lipgloss.NewStyle().
			Foreground(colorText)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	successStyle = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	warningStyle = lipgloss.NewStyle().
			Foreground(colorWarning)

	mutedStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	costStyle = lipgloss.NewStyle().
			Foreground(colorWarning).
			Bold(true)

	spinnerStyle = lipgloss.NewStyle().
			Foreground(colorPrimary)

	selectedStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)

	logoStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)

	// Mode badge styles (background color + white text)
	modeBadgeFastStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(colorSuccess).
				Bold(true).
				Padding(0, 1)

	modeBadgeBalancedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(colorPrimary).
				Bold(true).
				Padding(0, 1)

	modeBadgePowerStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(colorWarning).
				Bold(true).
				Padding(0, 1)
)

const logo = `
 ┏┳┓┏━┓┃┏ ┏━╸╻ ╻┏━┓┏┓╻╺┳┓
 ┃┃┃┣━┫┣┻┓┣╸ ┃╻┃┣━┫┃┗┫ ┃┃
 ╹ ╹╹ ╹╹ ╹┗━╸┗┻┛╹ ╹╹ ╹╺┻┛`
