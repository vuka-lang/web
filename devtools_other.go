//go:build !unix

package web

import "os/exec"

func ownGroup(*exec.Cmd) {}

func terminate(c *exec.Cmd) { c.Process.Kill() }

func kill(c *exec.Cmd) { c.Process.Kill() }
