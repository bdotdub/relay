package relay

import (
	"os"
	"syscall"
)

func reloadCurrentProcess() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(executable, os.Args, os.Environ())
}
