package core

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionConcurrentAppendsRemainValidJSONL(t *testing.T) {
	session, err := NewSession(t.TempDir(), "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}

	const (
		writers = 8
		rows    = 20
	)
	payload := strings.Repeat("x", 8*1024)
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rows; i++ {
				if err := session.AppendMessage(provider.Message{
					Role:    provider.RoleAssistant,
					Content: []provider.Content{provider.TextBlock{Text: payload}},
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("append message: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	lineCount := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 32*1024)
	for scanner.Scan() {
		lineCount++
		if !json.Valid(scanner.Bytes()) {
			t.Fatalf("line %d is not valid JSON", lineCount)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if want := 1 + writers*rows; lineCount != want {
		t.Fatalf("line count = %d, want %d", lineCount, want)
	}
}
