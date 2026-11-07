//go:build !windows

package mediadecode

import "os/exec"

func hideDecoderWindow(*exec.Cmd) {}
