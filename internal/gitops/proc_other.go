//go:build !unix

package gitops

import "os/exec"

func killTree(*exec.Cmd) {}
