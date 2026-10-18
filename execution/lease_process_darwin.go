//go:build darwin

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Normal runner cleanup can acknowledge a joined tree if ps is unavailable.
func leasePlatformUnsupported() bool { return true }

type darwinLeaseProcess struct {
	group int
	state string
	start string
}

func darwinLeaseRows(pid int) (map[int]darwinLeaseProcess, error) {
	args := []string{"-axo", "pid=,pgid=,stat=,lstart="}
	if pid > 0 {
		args = []string{"-o", "pid=,pgid=,stat=,lstart=", "-p", strconv.Itoa(pid)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	rows := map[int]darwinLeaseProcess{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 8 {
			return nil, fmt.Errorf("invalid process observation")
		}
		id, idErr := strconv.Atoi(fields[0])
		group, groupErr := strconv.Atoi(fields[1])
		if idErr != nil || groupErr != nil {
			return nil, fmt.Errorf("invalid process identity")
		}
		rows[id] = darwinLeaseProcess{group: group, state: fields[2], start: strings.Join(fields[3:], " ")}
	}
	return rows, nil
}

func leaseProcessRef(pid int) map[string]any {
	ref := map[string]any{"pid": json.Number(strconv.Itoa(pid)), "token": nil, "boot": "darwin", "group": nil}
	rows, err := darwinLeaseRows(pid)
	if row, ok := rows[pid]; err == nil && ok {
		ref["token"] = row.start
		ref["group"] = json.Number(strconv.Itoa(row.group))
	} else if group, err := syscall.Getpgid(pid); err == nil {
		ref["group"] = json.Number(strconv.Itoa(group))
	}
	return ref
}

func leaseProcessAlive(ref map[string]any, group bool) bool {
	if boot := ref["boot"]; boot != nil && boot != "darwin" {
		return true
	}
	pid, ok := jsonInteger(ref["pid"])
	if !ok {
		return true
	}
	query := pid
	if group {
		query = 0
	}
	rows, err := darwinLeaseRows(query)
	if err != nil {
		// ps start times have one-second resolution. If unavailable, a
		// reused PID cannot be distinguished: keep the lease until PID/group
		// death or explicit runner cleanup, never reclaim just on timeout.
		if group {
			pgid, ok := jsonInteger(ref["group"])
			if !ok || pgid <= 0 {
				return true
			}
			pid = -pgid
		}
		return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	token, _ := ref["token"].(string)
	if row, ok := rows[pid]; ok && !strings.HasPrefix(row.state, "Z") && !strings.HasPrefix(row.state, "X") && (token == "" || row.start == token) {
		return true
	}
	if group {
		pgid, ok := jsonInteger(ref["group"])
		if !ok || pgid <= 0 {
			return true
		}
		for _, row := range rows {
			if row.group == pgid && !strings.HasPrefix(row.state, "Z") && !strings.HasPrefix(row.state, "X") {
				return true
			}
		}
	}
	return false
}
