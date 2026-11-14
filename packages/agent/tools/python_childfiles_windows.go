//go:build windows

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func attachChildFiles(cmd *exec.Cmd, files []*os.File) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	handles := make([]string, len(files))
	for i, f := range files {
		h := windows.Handle(f.Fd())
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, fmt.Errorf("mark child file %d inheritable: %w", i, err)
		}
		cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(h))
		handles[i] = strconv.FormatUint(uint64(h), 10)
	}
	return []string{"ZUT_CODEMODE_FDS=" + strings.Join(handles, ",")}, nil
}
