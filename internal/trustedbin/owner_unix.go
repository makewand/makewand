//go:build !windows

package trustedbin

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// inUserNamespace is false in the initial namespace, whose uid_map is the
// identity map; there a genuinely nobody-owned file is not trusted.
func inUserNamespace() bool {
	b, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return false
	}
	return strings.Join(strings.Fields(string(b)), " ") != "0 0 4294967295"
}

func overflowUID() uint32 {
	if b, err := os.ReadFile("/proc/sys/kernel/overflowuid"); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil {
			return uint32(v)
		}
	}
	return 65534
}

func checkOwnership(path string, info os.FileInfo, allowCurrentUser bool) error {
	// Sticky world-writable dirs (/tmp) still let any user plant files.
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group/world writable", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: ownership unavailable", path)
	}
	// Inside a user namespace (nested bwrap, containers, sandboxed agent
	// shells) root-owned host files appear as the kernel overflow uid. Such
	// unmapped owners are equally out of the current user's reach.
	if st.Uid == 0 || (st.Uid == overflowUID() && int(st.Uid) != os.Geteuid() && inUserNamespace()) {
		return nil
	}
	if allowCurrentUser && int(st.Uid) == os.Geteuid() {
		return nil
	}
	return fmt.Errorf("%s is owned by uid %d, not root", path, st.Uid)
}
