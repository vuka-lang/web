//go:build unix

package web

import (
	"os/exec"
	"syscall"
)

// ownGroup starts c in a process group of its own, which terminate and kill
// signal whole: a tool's children too.
func ownGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminate(c *exec.Cmd) { syscall.Kill(-c.Process.Pid, syscall.SIGTERM) }

func kill(c *exec.Cmd) { syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
