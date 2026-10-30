package modes

import (
	"context"

	"github.com/bnema/zut/packages/agent/session"
)

func (i *Interactive) runCompact() {
	i.portSettingsCall("compact", func(ctx context.Context, port session.Port) error { return port.Compact(ctx) })
}
