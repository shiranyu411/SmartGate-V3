//go:build linux

package worker

import (
	"os/exec"
	"syscall"
)

func configureNodeProbeProcess(command *exec.Cmd) {
	// A daemon crash must not leave an untracked temporary core running.
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
