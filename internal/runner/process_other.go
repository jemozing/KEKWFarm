//go:build !unix

package runner

import "os/exec"

func configureProcess(*exec.Cmd) {}
