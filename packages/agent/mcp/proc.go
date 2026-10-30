package mcp

import (
	"github.com/bnema/zut/packages/agent/internal/procgroup"
	"os/exec"
)

type processGuard struct{ *procgroup.Guard }

func isolateProcess(cmd *exec.Cmd) { procgroup.Isolate(cmd) }
func newProcessGuard(cmd *exec.Cmd) (*processGuard, error) {
	guard, err := procgroup.New(cmd)
	if err != nil {
		return nil, err
	}
	return &processGuard{guard}, nil
}
func (g *processGuard) awaitLeaderExit() bool { return g.Guard.AwaitLeaderExit() }
func (g *processGuard) terminate(force bool)  { g.Guard.Terminate(force) }
func (g *processGuard) cleanup()              { g.Guard.Cleanup() }
