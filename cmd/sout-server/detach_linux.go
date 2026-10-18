//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setDetachedProcess 在 Linux 下创建全新 Session 与进程组，彻底脱离父进程
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
