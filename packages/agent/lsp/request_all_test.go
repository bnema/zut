package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDocumentOrderLSPProcess(t *testing.T) {
	address := os.Getenv("ZUT_DOCUMENT_ORDER_ADDR")
	if address == "" {
		return
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		os.Exit(2)
	}
	defer conn.Close()
	opened := false
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, err := ReadMessage(reader)
		if err != nil {
			return
		}
		var message rpcEnvelope
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		result := `null`
		switch message.Method {
		case "initialize":
			result = `{"capabilities":{}}`
		case "textDocument/didOpen":
			opened = true
			continue
		case "test/navigation":
			if os.Getenv("ZUT_DOCUMENT_ORDER_BLOCK") == "1" {
				_, _ = conn.Write([]byte{1})
				var ack [1]byte
				if _, err := conn.Read(ack[:]); err != nil {
					return
				}
			}
			result = fmt.Sprintf(`{"opened":%v}`, opened)
		}
		if len(message.ID) > 0 {
			_ = WriteMessage(os.Stdout, []byte(`{"jsonrpc":"2.0","id":`+string(message.ID)+`,"result":`+result+`}`))
		}
	}
}

func TestRequestAllOpensDocumentAfterIdleRestart(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	servers := map[string]ServerConfig{}
	for _, id := range []string{"a", "b"} {
		block := "0"
		if id == "a" {
			block = "1"
		}
		servers[id] = ServerConfig{Command: os.Args[0], Args: []string{"-test.run=^TestDocumentOrderLSPProcess$"}, FileTypes: []string{".go"}, Env: map[string]string{"ZUT_DOCUMENT_ORDER_ADDR": listener.Addr().String(), "ZUT_DOCUMENT_ORDER_BLOCK": block}}
	}
	raw, _ := json.Marshal(map[string]any{"autoDetect": false, "servers": servers})
	if err := os.WriteFile(filepath.Join(root, "lsp.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Prewarm both servers to reproduce the cached second-server eviction.
	var connections []net.Conn
	for _, id := range []string{"a", "b"} {
		if _, err := m.Capabilities(ctx, root, path, id); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		connections = append(connections, conn)
	}
	ws, _ := m.workspace(root)
	m.mu.Lock()
	previous := ws.clients["b"]
	m.mu.Unlock()
	result := make(chan []Response, 1)
	go func() { result <- m.RequestAll(ctx, root, path, "test/navigation", nil) }()
	var ready [1]byte
	if _, err := connections[0].Read(ready[:]); err != nil {
		t.Fatal(err)
	}
	m.sweepIdle(time.Now().Add(10 * time.Minute))
	select {
	case <-previous.done:
	default:
		t.Fatal("second idle server not evicted")
	}
	if _, err := connections[0].Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var responses []Response
	select {
	case responses = <-result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(responses) != 2 {
		t.Fatalf("responses = %+v", responses)
	}
	for _, response := range responses {
		if response.Error != nil {
			t.Fatalf("%s: %v", response.Server, response.Error)
		}
		var navigation struct {
			Opened bool `json:"opened"`
		}
		if err := json.Unmarshal(response.Result, &navigation); err != nil {
			t.Fatal(err)
		}
		if !navigation.Opened {
			t.Fatalf("%s received navigation before didOpen: %s", response.Server, response.Result)
		}
	}
	m.mu.Lock()
	restarted := ws.clients["b"] != previous
	m.mu.Unlock()
	if !restarted {
		t.Fatal("second server did not restart")
	}
	// Accept and close the replacement's liveness connection too.
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
