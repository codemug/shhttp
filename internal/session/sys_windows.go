package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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

type credential struct{}

func sysProcAttr(tty bool, cred *credential) *syscall.SysProcAttr { return nil }

func startPTY(cmd *exec.Cmd, attrs *syscall.SysProcAttr, cols, rows uint16) (*os.File, error) {
	return nil, errors.New("TTY sessions are not supported on Windows")
}

func resizePTY(f *os.File, cols, rows uint16) error {
	return errors.New("TTY sessions are not supported on Windows")
}

func lookupRunAs(spec string) (*credential, map[string]string, error) {
	return nil, nil, errors.New("run_as is not supported on Windows")
}

func reapOrphans(sessionIDs []string) int { return 0 }

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
