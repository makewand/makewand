package tui

// AppState represents the current interaction state of the app.
type AppState int

const (
	StateIdle          AppState = iota // waiting for user input
	StateStreaming                     // receiving AI response stream
	StateConfirmFiles                  // waiting for Y/N on file write
	StateConfirmDeps                   // waiting for Y/N on dependency install
	StateConfirmTests                  // waiting for Y/N on test execution
	StateInstallingDeps                // deps being installed
	StateRunningTests                  // tests running
	StateAutoFixing                    // auto-fix in progress
	StateQuitting                      // app shutting down
)

// String returns a human-readable name for the state.
func (s AppState) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateStreaming:
		return "Streaming"
	case StateConfirmFiles:
		return "ConfirmFiles"
	case StateConfirmDeps:
		return "ConfirmDeps"
	case StateConfirmTests:
		return "ConfirmTests"
	case StateInstallingDeps:
		return "InstallingDeps"
	case StateRunningTests:
		return "RunningTests"
	case StateAutoFixing:
		return "AutoFixing"
	case StateQuitting:
		return "Quitting"
	default:
		return "Unknown"
	}
}

// IsConfirming returns true if the app is in any confirmation state.
func (s AppState) IsConfirming() bool {
	return s == StateConfirmFiles || s == StateConfirmDeps || s == StateConfirmTests
}

// IsBusy returns true if the app is doing background work.
func (s AppState) IsBusy() bool {
	return s == StateStreaming || s == StateInstallingDeps || s == StateRunningTests || s == StateAutoFixing
}
