//go:build !unix

package exec

import "os/exec"

// setProcessGroup is a no-op where process groups are unavailable; WaitDelay
// still bounds how long Run can block after cancellation.
func setProcessGroup(cmd *exec.Cmd) {}
