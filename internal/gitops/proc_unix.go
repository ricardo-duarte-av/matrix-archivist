//go:build unix

package gitops

import (
	"os/exec"
	"syscall"
)

// killTree makes cancelling cmd kill its whole process group: git leaves helpers such as
// git-remote-https running when only git itself is killed.
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
