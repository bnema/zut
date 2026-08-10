package core

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionPersistsSanitizedRetryLifecycleOnUsage(t *testing.T) {
	const privateDetail = "private provider response body"

	session, err := NewSession(t.TempDir(), "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(textMessage(provider.RoleUser, "hello")); err != nil {
		t.Fatal(err)
	}
	session.RecordRetryLifecycle(RetryLifecycleRecord{
		Event: RetryLifecycleRequestFailed, Scope: RetryScopeAgent,
		Attempt: 1, MaxAttempts: 4, Reason: RetryReasonOverload,
	})
	session.RecordRetryLifecycle(RetryLifecycleRecord{
		Event: RetryLifecycleRetryScheduled, Scope: RetryScopeAgent,
		Attempt: 2, MaxAttempts: 4, Reason: RetryReasonOverload, DelayMS: 2000,
	})
	// RetryReason has a string representation for JSON, so the session layer
	// clamps unknown values rather than trusting callers with persisted text.
	session.RecordRetryLifecycle(RetryLifecycleRecord{
		Event: RetryLifecycleRequestFailed, Scope: RetryScopeProvider,
		Attempt: 1, MaxAttempts: 1, Reason: RetryReason(privateDetail), Terminal: true,
	})
	if err := session.AppendUsage(provider.Usage{InputTokens: 1}, provider.Usage{InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privateDetail) {
		t.Fatalf("session leaked untrusted lifecycle text: %s", raw)
	}

	var lifecycle []RetryLifecycleRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Type           string                 `json:"type"`
			RetryLifecycle []RetryLifecycleRecord `json:"retry_lifecycle"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "usage" {
			lifecycle = row.RetryLifecycle
		}
	}
	if len(lifecycle) != 3 {
		t.Fatalf("retry lifecycle = %#v, want 3 records", lifecycle)
	}
	if lifecycle[0].Reason != RetryReasonOverload || lifecycle[1].DelayMS != 2000 || lifecycle[2].Reason != RetryReasonUnknown || !lifecycle[2].Terminal {
		t.Fatalf("retry lifecycle = %#v", lifecycle)
	}

	snapshot, err := ReadSessionSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSessionSnapshot with retry metadata: %v", err)
	}
	if len(snapshot.Messages) != 1 || extractText(snapshot.Messages[0]) != "hello" {
		t.Fatalf("snapshot messages changed by retry metadata: %#v", snapshot.Messages)
	}
}

func TestSessionFlushRetryLifecycleWritesOnlyWhenPending(t *testing.T) {
	session, err := NewSession(t.TempDir(), "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(textMessage(provider.RoleUser, "hello")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.FlushRetryLifecycle(provider.Usage{}, provider.Usage{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("empty lifecycle flush changed session size: %d -> %d", before.Size(), after.Size())
	}

	session.RecordRetryLifecycle(RetryLifecycleRecord{
		Event: RetryLifecycleRequestFailed, Scope: RetryScopeAgent,
		Attempt: 1, MaxAttempts: 1, Reason: RetryReasonServer, Terminal: true,
	})
	if err := session.FlushRetryLifecycle(provider.Usage{}, provider.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"retry_lifecycle"`) || !strings.Contains(string(raw), `"terminal":true`) {
		t.Fatalf("terminal lifecycle was not flushed: %s", raw)
	}
}
