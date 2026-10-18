//go:build !linux && !windows && !darwin

package execution

import (
	"encoding/json"
	"strconv"
)

// No portable PID/start-time/tree proof: crash reclamation fails closed.
// Normal provider runners explicitly acknowledge joined process-tree cleanup.
func leasePlatformUnsupported() bool { return true }
func leaseProcessRef(pid int) map[string]any {
	return map[string]any{"pid": json.Number(strconv.Itoa(pid)), "token": nil, "boot": nil, "group": nil}
}
func leaseProcessAlive(map[string]any, bool) bool { return true }
