package telegram

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientErrorsNeverContainToken(t *testing.T) {
	const token = "42:VERY-SECRET"
	cause := errors.New("dial failed")
	c := NewClientWithBase(token, "http://127.0.0.1:1/")
	c.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}
	_, err := c.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "VERY-SECRET") || !errors.Is(err, cause) {
		t.Fatalf("GetMe error = %v", err)
	}
	if _, err := c.DownloadFile(context.Background(), "photos/x.jpg"); err == nil || strings.Contains(err.Error(), "VERY-SECRET") {
		t.Fatalf("DownloadFile error = %v", err)
	}
}

func TestSaveConfigIsPrivate(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("POSIX modes only")
	}
	home := t.TempDir()
	// A stale world-readable temp file must not keep its mode.
	if err := os.WriteFile(filepath.Join(home, "bot.json.tmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(home, Config{BotToken: "x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(ConfigPath(home))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bot.json mode = %v, %v", fi.Mode(), err)
	}
}
