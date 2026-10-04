//go:build unix

package session

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func shellCommand(command string) (path string, args []string) {
	return "/bin/sh", []string{"sh", "-c", command}
}

// Each session runs in its own process group, whose id is the pid of the
// session's process.

func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

func terminateGroup(pid int) (string, error) {
	return "SIGTERM", signalGroup(pid, syscall.SIGTERM)
}

func killGroup(pid int) {
	signalGroup(pid, syscall.SIGKILL)
}

// parseSignal accepts names such as "SIGINT", "INT" or "int".
func parseSignal(name string) (syscall.Signal, string, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if !strings.HasPrefix(n, "SIG") {
		n = "SIG" + n
	}
	sig := unix.SignalNum(n)
	if sig == 0 {
		return 0, "", fmt.Errorf("unknown signal %q", name)
	}
	return sig, n, nil
}

func exitInfo(ps *os.ProcessState) (code *int, signal string) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return nil, unix.SignalName(ws.Signal())
	}
	c := ps.ExitCode()
	return &c, ""
}
