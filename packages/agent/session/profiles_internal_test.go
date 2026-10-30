package session

import "testing"

// SetBusy makes the harness report a running turn.
func (h *Lane9Harness) SetBusy(t *testing.T) {
	t.Helper()
	m := newMockturnSeam(t)
	m.EXPECT().isBusy().Return(true).Maybe()
	m.EXPECT().turnOrigin().Return(InputOrigin("")).Maybe()
	m.EXPECT().turnInfo().Return(turnInfoProjection{}).Maybe()
	m.EXPECT().currentAgent().Return(h.agent).Maybe()
	if err := h.C.call(h.Ctx, func() { h.C.turnSeam = m }); err != nil {
		t.Fatal(err)
	}
}
