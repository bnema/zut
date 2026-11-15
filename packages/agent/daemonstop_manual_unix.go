//go:build !windows

package agent

const daemonManualStopCommand = "kill -TERM"
