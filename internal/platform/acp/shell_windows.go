//go:build windows

package acp

import "os/exec"

func shellCommand(s string) *exec.Cmd { return exec.Command("cmd", "/C", s) }
