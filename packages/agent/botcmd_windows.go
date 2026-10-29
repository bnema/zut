//go:build windows

package agent

import "github.com/bnema/zut/packages/agent/internal/detach"

// detachChild runs the bot in its own console-less process group (see internal/detach).
func init() { detachChild = detach.Configure }
