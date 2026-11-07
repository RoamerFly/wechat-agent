package mediadecode

import (
	"os/exec"
	"syscall"
)

func hideDecoderWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
