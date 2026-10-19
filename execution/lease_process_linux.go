package execution

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func leasePlatformUnsupported() bool { return false }
func processStat(pid string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return nil, err
	}
	split := strings.LastIndexByte(string(raw), ')')
	if split < 0 {
		return nil, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(raw[split+1:]))
	if len(fields) < 20 {
		return nil, errors.New("short process stat")
	}
	return fields, nil
}
func leaseProcessRef(pid int) map[string]any {
	ref := map[string]any{"pid": json.Number(strconv.Itoa(pid)), "token": nil, "boot": nil, "group": nil}
	fields, err := processStat(strconv.Itoa(pid))
	boot, bootErr := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err == nil && bootErr == nil {
		ref["token"] = fields[19]
		ref["boot"] = strings.TrimSpace(string(boot))
		ref["group"] = json.Number(fields[2])
	}
	return ref
}
func leaseProcessAlive(ref map[string]any, group bool) bool {
	token, ok := ref["token"].(string)
	if !ok {
		return true
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return true
	}
	if strings.TrimSpace(string(boot)) != ref["boot"] {
		return false
	}
	pid, ok := jsonInteger(ref["pid"])
	if !ok {
		return true
	}
	fields, err := processStat(strconv.Itoa(pid))
	if err == nil && fields[19] == token && fields[0] != "Z" && fields[0] != "X" {
		return true
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true
	}
	if group {
		wanted, ok := jsonInteger(ref["group"])
		if !ok {
			return true
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return true
		}
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			fields, err := processStat(entry.Name())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return true
			}
			g, _ := strconv.Atoi(fields[2])
			if g == wanted && fields[0] != "Z" && fields[0] != "X" {
				return true
			}
		}
	}
	return false
}
