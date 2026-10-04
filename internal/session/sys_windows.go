package session

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// On Windows only killing is supported, and only the session's own process
// is killed; process trees are not tracked yet.

func shellCommand(command string) (path string, args []string) {
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	return comspec, []string{"cmd", "/C", command}
}

func sysProcAttr() *syscall.SysProcAttr { return nil }

func kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func signalGroup(pid int, sig syscall.Signal) error {
	if sig != syscall.SIGKILL {
		return fmt.Errorf("only SIGKILL is supported on Windows")
	}
	return kill(pid)
}

func terminateGroup(pid int) (string, error) { return "SIGKILL", kill(pid) }

func killGroup(pid int) { kill(pid) }

func parseSignal(name string) (syscall.Signal, string, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "KILL" || n == "SIGKILL" {
		return syscall.SIGKILL, "SIGKILL", nil
	}
	return 0, "", fmt.Errorf("signal %q is not supported on Windows; only SIGKILL is", name)
}

func exitInfo(ps *os.ProcessState) (code *int, signal string) {
	c := ps.ExitCode()
	return &c, ""
}
