package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
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

	var cmd *exec.Cmd

	if content, err := p.ReadFile("package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal([]byte(content), &pkg) == nil {
			if _, ok := pkg.Scripts["dev"]; ok {
				cmd = exec.CommandContext(ctx, "npm", "run", "dev", "--", "--port", fmt.Sprintf("%d", port))
			} else if _, ok := pkg.Scripts["start"]; ok {
				cmd = exec.CommandContext(ctx, "npm", "start", "--", "--port", fmt.Sprintf("%d", port))
			}
		}
	}

	if cmd == nil {
		if _, err := p.ReadFile("manage.py"); err == nil {
			cmd = exec.CommandContext(ctx, "python", "manage.py", "runserver", fmt.Sprintf("127.0.0.1:%d", port))
		}
	}

	if cmd == nil {
		if _, err := p.ReadFile("main.py"); err == nil {
			cmd = exec.CommandContext(ctx, "python", "main.py")
		}
	}

	// Fallback: simple Python HTTP server for static sites
	if cmd == nil {
		cmd = exec.CommandContext(ctx, "python3", "-m", "http.server", "--bind", "127.0.0.1", fmt.Sprintf("%d", port))
	}

	cmd.Dir = p.Path
	cmd.Env = append(sanitizeExecEnv(cmd.Environ()),
		fmt.Sprintf("PORT=%d", port),
		"HOST=127.0.0.1",
	)
	setProcessGroup(cmd)
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
		killProcessGroup(s.cmd)
		s.cmd.Wait()
	}
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
