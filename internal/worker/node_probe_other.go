//go:build !linux

package worker

import "os/exec"

func configureNodeProbeProcess(_ *exec.Cmd) {}
