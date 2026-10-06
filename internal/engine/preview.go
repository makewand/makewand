package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/execution"
	"github.com/makewand/makewand/internal/processjob"
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
	done    chan struct{}
	waitErr error // published by closing done
	stop    sync.Once
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
		done:    make(chan struct{}),
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
		// Fallback: simple Python HTTP server for static sites. Run the
		// interpreter in isolated mode (-I): this drops the working directory
		// from sys.path, disables the user site dir, and ignores PYTHON* env,
		// so a project-local sitecustomize.py or an http/ package cannot be
		// imported and execute host code. http.server still serves the project
		// directory (cmd.Dir / os.getcwd()) as static files.
		command = "python3"
		args = []string{"-I", "-m", "http.server", "--bind", "127.0.0.1", fmt.Sprintf("%d", port)}
	}

	// Project-defined scripts execute inside an isolation wrapper by default.
	if kind != previewStatic {
		wrappedCommand, wrappedArgs, wrapErr := previewWrapProjectCmd(p.Path, command, args, p.unsafeHostAuth)
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
	setProcessGroup(cmd)
	capture := processjob.NewCapture(maxOutputBytes, func() { killProcessGroup(cmd) })
	cmd.Stdout, cmd.Stderr = capture.Stdout(), capture.Stderr()
	server.cmd = cmd

	cleanupProcess, err := processjob.Start(cmd)
	if err != nil {
		contextErr := ctx.Err()
		cancel()
		if contextErr != nil {
			return nil, fmt.Errorf("start preview server: %w", errors.Join(contextErr, err))
		}
		return nil, fmt.Errorf("start preview server: %w", err)
	}
	go func() {
		waitErr := cmd.Wait()
		killProcessGroup(cmd)
		cleanupProcess()
		if capture.Exceeded() {
			waitErr = &execution.UnknownOutcomeError{Err: processjob.OutputLimitError()}
		} else if errors.Is(waitErr, exec.ErrWaitDelay) {
			waitErr = &execution.UnknownOutcomeError{Err: waitErr}
		}
		server.waitErr = waitErr
		close(server.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
		case <-server.done:
		}
	}()
	if err := previewWaitForPort(ctx, port, previewReadyTimeout); err != nil {
		server.Stop()
		stderrMsg := capture.StderrString()
		if len(stderrMsg) > previewStartupStderrLimit {
			stderrMsg = stderrMsg[:previewStartupStderrLimit]
		}
		stderrMsg = strings.TrimSpace(stderrMsg)
		if stderrMsg != "" {
			stderrMsg = strings.Join(strings.Fields(stderrMsg), " ")
			return nil, fmt.Errorf("preview server did not become ready on 127.0.0.1:%d: %w; startup stderr: %s", port, err, stderrMsg)
		}
		return nil, fmt.Errorf("preview server did not become ready on 127.0.0.1:%d: %w", port, err)
	}
	select {
	case <-server.done:
		cancel()
		if server.waitErr != nil {
			return nil, fmt.Errorf("preview server exited before readiness: %w", server.waitErr)
		}
		return nil, fmt.Errorf("preview server exited before readiness")
	default:
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
	if s == nil {
		return
	}
	s.stop.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			killProcessGroup(s.cmd)
		}
	})
	if s.done != nil {
		<-s.done
	}
}

// Done closes when the process and its inherited output pipes have finished.
func (s *PreviewServer) Done() <-chan struct{} { return s.done }

// Err returns the final execution result after Done, or nil while running.
// Output overflow and incomplete inherited pipes are uncertain outcomes.
func (s *PreviewServer) Err() error {
	select {
	case <-s.done:
		return s.waitErr
	default:
		return nil
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
