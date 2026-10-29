//go:build !windows

package agent

import "github.com/bnema/zut/packages/agent/internal/detach"

// detachChild runs the bot in a new session (see internal/detach).
func init() { detachChild = detach.Configure }
