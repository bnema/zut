package agent

import (
	"context"
	"time"
)

const headlessShutdownTimeout = 5 * time.Second

func closeSubagentRuntimeFresh(rt *subagentRuntime) error {
	if rt == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), headlessShutdownTimeout)
	defer cancel()
	return rt.Close(shutdownCtx)
}
