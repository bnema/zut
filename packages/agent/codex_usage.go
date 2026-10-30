package agent

import "github.com/bnema/zut/packages/core"

func (r Resolved) newInteractiveAgent() *core.Agent {
	return r.NewAgent()
}
