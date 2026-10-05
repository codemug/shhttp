package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// reapOrphans kills processes left behind by sessions of a previous server
// that crashed. Every session process carries SHHTTP_SESSION_ID in its
// environment, and children inherit it, so they can be found even after
// their process group leader died. It returns how many it killed.
func reapOrphans(sessionIDs []string) int {
	if len(sessionIDs) == 0 {
		return 0
	}
	wanted := map[string]bool{}
	for _, id := range sessionIDs {
		wanted[sessionEnv+"="+id] = true
	}
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	killed := 0
	self := os.Getpid()
	for _, dir := range dirs {
		pid, err := strconv.Atoi(filepath.Base(dir))
		if err != nil || pid == self {
			continue
		}
		env, err := os.ReadFile(filepath.Join(dir, "environ"))
		if err != nil {
			continue // gone, or another user's process
		}
		for _, kv := range bytes.Split(env, []byte{0}) {
			if wanted[string(kv)] {
				if syscall.Kill(pid, syscall.SIGKILL) == nil {
					killed++
				}
				break
			}
		}
	}
	return killed
}
