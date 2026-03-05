package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

var (
	previewCommandContext = exec.CommandContext
	previewFindFreePort   = findFreePort
	previewWaitForPort    = waitForPreviewPort
	previewWrapProjectCmd = wrapPreviewProjectCommand
	previewDialTimeout    = net.DialTimeout
	previewReadyTimeout   = 12 * time.Second
)

const previewStartupStderrLimit = 8 << 10

// PreviewServer manages a development preview server.
type PreviewServer struct {
	project *Project
	cmd     *exec.Cmd
	port    int
	cancel  context.CancelFunc
}

// StartPreview starts a development server for the project.
func (p *Project) StartPreview(ctx context.Context, allowProjectScripts bool) (*PreviewServer, error) {
	type previewCommandKind int
	const (
		previewStatic previewCommandKind = iota
		previewNodeDev
		previewNodeStart
		previewDjango
		previewPythonMain
	)

	kind := previewStatic

	if content, err := p.ReadFile("package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal([]byte(content), &pkg) == nil {
			if _, ok := pkg.Scripts["dev"]; ok {
				if !allowProjectScripts {
					return nil, fmt.Errorf("refusing to run project script %q without --allow-project-scripts", "npm run dev")
				}
				kind = previewNodeDev
			} else if _, ok := pkg.Scripts["start"]; ok {
				if !allowProjectScripts {
					return nil, fmt.Errorf("refusing to run project script %q without --allow-project-scripts", "npm start")
				}
				kind = previewNodeStart
			}
		}
	}

	if kind == previewStatic {
		if _, err := p.ReadFile("manage.py"); err == nil {
			if !allowProjectScripts {
				return nil, fmt.Errorf("refusing to run project script %q without --allow-project-scripts", "python manage.py runserver")
			}
			kind = previewDjango
		}
	}

	if kind == previewStatic {
		if _, err := p.ReadFile("main.py"); err == nil {
			if !allowProjectScripts {
				return nil, fmt.Errorf("refusing to run project script %q without --allow-project-scripts", "python main.py")
			}
			kind = previewPythonMain
		}
	}

	port, err := previewFindFreePort()
	if err != nil {
		return nil, fmt.Errorf("find free port: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	server := &PreviewServer{
		project: p,
		port:    port,
		cancel:  cancel,
	}

	var command string
	var args []string
	switch kind {
	case previewNodeDev:
		command = "npm"
		args = []string{"run", "dev", "--", "--port", fmt.Sprintf("%d", port)}
	case previewNodeStart:
		command = "npm"
		args = []string{"start", "--", "--port", fmt.Sprintf("%d", port)}
	case previewDjango:
		command = "python"
		args = []string{"manage.py", "runserver", fmt.Sprintf("127.0.0.1:%d", port)}
	case previewPythonMain:
		command = "python"
		args = []string{"main.py"}
	default:
		// Fallback: simple Python HTTP server for static sites.
		command = "python3"
		args = []string{"-m", "http.server", "--bind", "127.0.0.1", fmt.Sprintf("%d", port)}
	}

	// Project-defined scripts execute inside an isolation wrapper by default.
	if kind != previewStatic {
		wrappedCommand, wrappedArgs, wrapErr := previewWrapProjectCmd(p.Path, command, args)
		if wrapErr != nil {
			cancel()
			return nil, wrapErr
		}
		command = wrappedCommand
		args = wrappedArgs
	}
	cmd := previewCommandContext(ctx, command, args...)

	cmd.Dir = p.Path
	cmd.Env = append(sanitizeExecEnv(cmd.Environ()),
		fmt.Sprintf("PORT=%d", port),
		"HOST=127.0.0.1",
	)
	startupStderr := &limitedWriter{limit: previewStartupStderrLimit}
	cmd.Stderr = startupStderr
	setProcessGroup(cmd)
	server.cmd = cmd

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start preview server: %w", err)
	}
	if err := previewWaitForPort(ctx, port, previewReadyTimeout); err != nil {
		cancel()
		killProcessGroup(cmd)
		_ = cmd.Wait()
		stderrMsg := strings.TrimSpace(startupStderr.String())
		if stderrMsg != "" {
			stderrMsg = strings.Join(strings.Fields(stderrMsg), " ")
			return nil, fmt.Errorf("preview server did not become ready on 127.0.0.1:%d: %w; startup stderr: %s", port, err, stderrMsg)
		}
		return nil, fmt.Errorf("preview server did not become ready on 127.0.0.1:%d: %w", port, err)
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

func waitForPreviewPort(ctx context.Context, port int, timeout time.Duration) error {
	if port <= 0 {
		return fmt.Errorf("invalid preview port %d", port)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	lastErr := error(nil)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := previewDialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		select {
		case <-waitCtx.Done():
			if lastErr == nil {
				lastErr = waitCtx.Err()
			}
			return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}
