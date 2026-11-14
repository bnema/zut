//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func attachChildFiles(cmd *exec.Cmd, files []*os.File) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	cmd.ExtraFiles = files
	fds := make([]string, len(files))
	for i := range files {
		fds[i] = strconv.Itoa(3 + i)
	}
	return []string{"ZUT_CODEMODE_FDS=" + strings.Join(fds, ",")}, nil
}
