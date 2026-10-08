//go:build windows

package processjob

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var jobs sync.Map // *exec.Cmd -> *jobLease; avoids signalling a reused PID

type jobLease struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (j *jobLease) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle != 0 {
		_ = windows.CloseHandle(j.handle) // KILL_ON_JOB_CLOSE terminates descendants
		j.handle = 0
	}
}

func (j *jobLease) kill() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return os.ErrProcessDone
	}
	return windows.TerminateJobObject(j.handle, 1)
}

func start(cmd *exec.Cmd, options Options) (func(), error) {
	if err := prepare(cmd); err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process Job: %w", err)
	}
	lease := &jobLease{handle: job}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if options.MaxProcesses > 0 {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		limits.BasicLimitInformation.ActiveProcessLimit = options.MaxProcesses
	}
	if options.MemoryBytes > 0 {
		limits.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		limits.JobMemoryLimit = options.MemoryBytes
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		lease.close()
		return nil, fmt.Errorf("configure process Job: %w", err)
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	// Start suspended so no descendant can race ahead of job assignment. An
	// inherited BREAKAWAY request is refused instead of weakening ownership.
	attr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
	attr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr = &attr
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return Kill(cmd) }
	}
	if err := cmd.Start(); err != nil {
		lease.close()
		return nil, err
	}
	abort := func(cause error) (func(), error) {
		_ = cmd.Process.Kill()
		lease.close()
		_ = cmd.Wait()
		jobs.CompareAndDelete(cmd, lease)
		return nil, cause
	}
	pid := cmd.Process.Pid
	if pid < 1 || uint64(pid) > math.MaxUint32 {
		return abort(fmt.Errorf("invalid subprocess identity"))
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: pid range checked above.
	if err != nil {
		return abort(fmt.Errorf("open suspended subprocess: %w", err))
	}
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(process)
		return abort(fmt.Errorf("assign subprocess Job: %w", err))
	}
	jobs.Store(cmd, lease)
	if err := resumePrimaryThread(uint32(pid)); err != nil { //nolint:gosec // G115: pid range checked above.
		_ = windows.CloseHandle(process)
		return abort(fmt.Errorf("resume supervised subprocess: %w", err))
	}
	// Waiting on an independent process HANDLE avoids cmd.Wait's pipe-draining
	// dependency. On normal leader exit, closing the job kills descendants
	// (including new process groups) before inherited stdout can hold Wait open.
	go func() {
		_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
		lease.close()
		_ = windows.CloseHandle(process)
		jobs.CompareAndDelete(cmd, lease)
	}()
	return func() {
		lease.close()
		jobs.CompareAndDelete(cmd, lease)
	}, nil
}

func resumePrimaryThread(pid uint32) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
		if err != nil {
			lastErr = err
			continue
		}
		entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
		found := false
		for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
			if entry.OwnerProcessID != pid {
				continue
			}
			found = true
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				_ = windows.CloseHandle(snapshot)
				return openErr
			}
			previous, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			_ = windows.CloseHandle(snapshot)
			if resumeErr != nil {
				return resumeErr
			}
			if previous != 1 {
				return fmt.Errorf("unexpected primary-thread suspend count %d", previous)
			}
			return nil
		}
		_ = windows.CloseHandle(snapshot)
		if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			lastErr = err
		} else if !found {
			lastErr = fmt.Errorf("suspended subprocess primary thread is unavailable")
		}
	}
	return lastErr
}

// Kill terminates the captured Job rather than using taskkill against a PID.
// Processes initialized outside Start retain only ordinary process cancellation.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if lease, ok := jobs.Load(cmd); ok {
		return lease.(*jobLease).kill()
	}
	return cmd.Process.Kill()
}
