package engine

// UnsafeHostExecEvent records one command that actually executed directly on
// the host because of the MAKEWAND_UNSAFE_HOST_EXEC opt-in (as opposed to
// running inside the bubblewrap sandbox). Every such execution is reported to
// the authorization's Audit hook — not just the first — so an audit trail
// exists for each host command, per the acknowledgment's terms.
type UnsafeHostExecEvent struct {
	Context string // "verification" (deps/tests/auto-fix) or "preview"
	Command string
	Args    []string
	Dir     string
	Source  string // authorization source, e.g. "config-ack", "interactive-ack"
}

// UnsafeHostExecAuthorization is the app layer's explicit answer to whether
// the MAKEWAND_UNSAFE_HOST_EXEC=1 request may take effect. The engine never
// reads user config itself: the caller (cmd/tui) resolves the one-time
// acknowledgment state and passes the result down. The zero value means "not
// acknowledged", so an unwired call site fails closed — the environment
// variable alone no longer enables host execution.
type UnsafeHostExecAuthorization struct {
	// Acknowledged is true only after the user completed the one-time
	// responsibility acknowledgment (or an equivalent explicit gate, e.g. the
	// casefix CLI's own opt-in prompt).
	Acknowledged bool
	// Source describes where the acknowledgment came from, for audit lines.
	Source string
	// Audit, when set, receives every command that executes on the host under
	// this authorization.
	Audit func(UnsafeHostExecEvent)
}

// audit reports one host execution to the Audit hook, stamping the source.
func (a UnsafeHostExecAuthorization) audit(ev UnsafeHostExecEvent) {
	if a.Audit == nil {
		return
	}
	ev.Source = a.Source
	a.Audit(ev)
}

// SetUnsafeHostExecAuthorization attaches the resolved host-execution
// authorization to this project. Temporary verification clones created via
// CloneToTemp inherit it.
func (p *Project) SetUnsafeHostExecAuthorization(auth UnsafeHostExecAuthorization) {
	if p == nil {
		return
	}
	p.unsafeHostAuth = auth
}

// UnsafeHostExecAuth returns the authorization attached to this project.
func (p *Project) UnsafeHostExecAuth() UnsafeHostExecAuthorization {
	if p == nil {
		return UnsafeHostExecAuthorization{}
	}
	return p.unsafeHostAuth
}
