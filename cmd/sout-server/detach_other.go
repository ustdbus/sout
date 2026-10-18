//go:build !linux

package main

import "os/exec"

// setDetachedProcess 在非 Linux 平台下保持空实现以支持跨平台编译
func setDetachedProcess(cmd *exec.Cmd) {
}
