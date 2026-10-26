//go:build linux

package main

import (
	"bytes"
	"os"
	"strconv"
)

// processZombie detects exited but unreaped processes. kill(pid, 0) alone
// returns success for zombies on Linux, so waiting only for ESRCH can cause
// spurious 20-second shutdown and rollback failures, especially in containers
// whose PID 1 does not promptly reap adopted children.
func processZombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false // Be conservative if procfs is unavailable.
	}
	// comm is enclosed in parentheses and may itself contain spaces and
	// parentheses. The state is the byte after the LAST ") ".
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && end+2 < len(stat) && stat[end+1] == ' ' &&
		(stat[end+2] == 'Z' || stat[end+2] == 'X' || stat[end+2] == 'x')
}
