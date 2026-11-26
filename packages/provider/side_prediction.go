package provider

// SidePredictionSupporter lets a client opt out of optional side requests,
// such as next-prompt prediction, that end in a synthetic user message.
type SidePredictionSupporter interface {
	SupportsSidePrediction() bool
}

// SupportsSidePrediction reports whether c may receive optional side
// requests. Clients that do not implement SidePredictionSupporter support
// them by default.
func SupportsSidePrediction(c Client) bool {
	if c == nil {
		return false
	}
	if s, ok := c.(SidePredictionSupporter); ok {
		return s.SupportsSidePrediction()
	}
	return true
}

// SupportsSidePrediction is false for GitHub Copilot: a request ending in a
// user message is billed as a premium user-initiated request, so a hidden
// prediction would spend the user's premium quota.
func (c *copilotClient) SupportsSidePrediction() bool { return false }

// SupportsSidePrediction delegates to the wrapped client.
func (c *RefreshingClient) SupportsSidePrediction() bool {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	return SupportsSidePrediction(inner)
}

// SupportsSidePrediction delegates to the wrapped client.
func (r *renamedClient) SupportsSidePrediction() bool { return SupportsSidePrediction(r.inner) }
