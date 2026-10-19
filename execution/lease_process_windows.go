//go:build windows

package execution

import (
	"encoding/json"
	"errors"
	"strconv"

	"golang.org/x/sys/windows"
)

// A process handle proves one lifetime, not that every Job descendant exited.
// Normal runner cleanup explicitly acknowledges the joined process tree.
func leasePlatformUnsupported() bool { return true }

func windowsLeaseSnapshot(pid int) (live bool, known bool, token string) {
	if pid <= 0 || uint64(pid) > uint64(0xffffffff) {
		return false, false, ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false, errors.Is(err, windows.ERROR_INVALID_PARAMETER), ""
	}
	defer windows.CloseHandle(h)
	status, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false, false, ""
	}
	if status == windows.WAIT_OBJECT_0 {
		return false, true, ""
	}
	if status != uint32(windows.WAIT_TIMEOUT) {
		return false, false, ""
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		token = strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10)
	}
	return true, true, token
}

func leaseProcessRef(pid int) map[string]any {
	ref := map[string]any{"pid": json.Number(strconv.Itoa(pid)), "token": nil, "boot": "windows", "group": nil}
	if _, _, token := windowsLeaseSnapshot(pid); token != "" {
		ref["token"] = token
	}
	return ref
}

func leaseProcessAlive(ref map[string]any, group bool) bool {
	if group {
		// A crashed owner may leave live descendants after its leader exits.
		// TTL and a dead leader cannot substitute for runner Job cleanup.
		return true
	}
	if boot := ref["boot"]; boot != nil && boot != "windows" {
		return true
	}
	pid, ok := jsonInteger(ref["pid"])
	if !ok {
		return true
	}
	live, known, token := windowsLeaseSnapshot(pid)
	if !known {
		return true
	}
	if !live {
		return false
	}
	previous, _ := ref["token"].(string)
	return token == "" || previous == "" || token == previous
}
