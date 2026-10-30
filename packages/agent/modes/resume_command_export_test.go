package modes

import "testing"

// NewResumeTestClient shares the generated mock with external integration tests.
func NewResumeTestClient(t *testing.T) *mockSessionClient { return newMockSessionClient(t) }
