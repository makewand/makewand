package engine

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// PreviewServer manages a development preview server.
type PreviewServer struct {
	project *Project
	cmd     *exec.Cmd
	port    int
	cancel  context.CancelFunc
}

// StartPreview starts a development server for the project.
func (p *Project) StartPreview(ctx context.Context) (*PreviewServer, error) {
	port, err := findFreePort()
	if err != nil {
		return nil, fmt.Errorf("find free port: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	server := &PreviewServer{
		project: p,
		port:    port,
		cancel:  cancel,
	}

	// Detect project type and start appropriate server
	var cmd *exec.Cmd

	if content, err := p.ReadFile("package.json"); err == nil {
		if strings.Contains(content, `"dev"`) {
			cmd = exec.CommandContext(ctx, "npm", "run", "dev", "--", "--port", fmt.Sprintf("%d", port))
		} else if strings.Contains(content, `"start"`) {
			cmd = exec.CommandContext(ctx, "npm", "start")
		}
	}

	if cmd == nil {
		if _, err := p.ReadFile("manage.py"); err == nil {
			cmd = exec.CommandContext(ctx, "python", "manage.py", "runserver", fmt.Sprintf("0.0.0.0:%d", port))
		}
	}

	if cmd == nil {
		if _, err := p.ReadFile("main.py"); err == nil {
			cmd = exec.CommandContext(ctx, "python", "main.py")
		}
	}

	// Fallback: simple Python HTTP server for static sites
	if cmd == nil {
		cmd = exec.CommandContext(ctx, "python3", "-m", "http.server", fmt.Sprintf("%d", port))
	}

	cmd.Dir = p.Path
	server.cmd = cmd

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start preview server: %w", err)
	}

	return server, nil
}

// URL returns the preview URL.
func (s *PreviewServer) URL() string {
	return fmt.Sprintf("http://localhost:%d", s.port)
}

// Port returns the preview port.
func (s *PreviewServer) Port() int {
	return s.port
}

// Stop stops the preview server.
func (s *PreviewServer) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
