//go:build linux

package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadClipboardImageWaylandUsesMIMEPriority(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "listed")
	writeClipboardTestHelper(t, dir, "wl-paste", `#!/bin/sh
if [ "$1" = "--list-types" ]; then
  : > "$CLIPBOARD_TEST_STATE"
  printf 'image/jpeg\nimage/png\nimage/bmp\n'
  exit 0
fi
if [ "$1" = "--type" ]; then
  if [ ! -f "$CLIPBOARD_TEST_STATE" ]; then
    exit 1
  fi
  case "$2" in
    image/png) printf 'png-bytes' ;;
    image/jpeg) printf 'jpeg-bytes' ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 1
`)
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")
	t.Setenv("CLIPBOARD_TEST_STATE", state)

	image, ok, err := ReadClipboardImage(context.Background())
	if err != nil {
		t.Fatalf("ReadClipboardImage() error = %v", err)
	}
	if !ok {
		t.Fatal("ReadClipboardImage() reported no image")
	}
	if image.MimeType != "image/png" {
		t.Fatalf("MIME type = %q, want image/png", image.MimeType)
	}
	if string(image.Data) != "png-bytes" {
		t.Fatalf("image data = %q, want png-bytes", image.Data)
	}
}

func TestReadClipboardImageFallsBackToX11Helpers(t *testing.T) {
	dir := t.TempDir()
	writeClipboardTestHelper(t, dir, "wl-paste", `#!/bin/sh
if [ "$1" = "--list-types" ]; then
  printf 'text/plain\n'
fi
`)
	writeClipboardTestHelper(t, dir, "xclip", `#!/bin/sh
case "$*" in
  *"image/gif"*) printf 'gif-bytes' ;;
  *) exit 1 ;;
esac
`)
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", ":test")

	image, ok, err := ReadClipboardImage(context.Background())
	if err != nil {
		t.Fatalf("ReadClipboardImage() error = %v", err)
	}
	if !ok {
		t.Fatal("ReadClipboardImage() reported no image")
	}
	if image.MimeType != "image/gif" {
		t.Fatalf("MIME type = %q, want image/gif", image.MimeType)
	}
	if string(image.Data) != "gif-bytes" {
		t.Fatalf("image data = %q, want gif-bytes", image.Data)
	}
}

func TestReadClipboardImageUnavailableIsNoImage(t *testing.T) {
	dir := t.TempDir()
	writeClipboardTestHelper(t, dir, "wl-paste", `#!/bin/sh
printf 'clipboard helper failure' >&2
exit 1
`)
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")

	image, ok, err := ReadClipboardImage(context.Background())
	if err != nil {
		t.Fatalf("ReadClipboardImage() error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("ReadClipboardImage() returned image %#v", image)
	}
	if image.MimeType != "" || len(image.Data) != 0 {
		t.Fatalf("empty image = %#v", image)
	}

	path, data, ok, err := ReadClipboardImagePNG()
	if err != nil || ok || path != "" || data != nil {
		t.Fatalf("ReadClipboardImagePNG() = (%q, %v, %v, %v), want empty result", path, data, ok, err)
	}
}

func TestWriteClipboardTextWaylandUsesWlCopy(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "clipboard-payload")
	writeClipboardTestHelper(t, dir, "wl-copy", `#!/bin/sh
if [ "$1" != "--type" ] || [ "$2" != "text/plain" ]; then
  exit 1
fi
exec /bin/cat > "$CLIPBOARD_TEST_PAYLOAD"
`)
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	t.Setenv("DISPLAY", "")
	t.Setenv("CLIPBOARD_TEST_PAYLOAD", payload)

	if err := WriteClipboardText("https://example.com/oauth?state=xyz"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "https://example.com/oauth?state=xyz" {
		t.Fatalf("clipboard payload = %q", got)
	}
}

func TestRunClipboardImageCommandEnforcesSizeLimit(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	writeClipboardTestHelper(t, dir, "clipboard-test-helper", `#!/bin/sh
exec /bin/cat "$CLIPBOARD_TEST_PAYLOAD"
`)
	t.Setenv("PATH", dir)
	t.Setenv("CLIPBOARD_TEST_PAYLOAD", payload)

	if err := os.WriteFile(payload, bytes.Repeat([]byte{'x'}, maxClipboardImageBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	data, ok := runClipboardImageCommand(context.Background(), clipboardImageCommand{name: "clipboard-test-helper"})
	if !ok || len(data) != maxClipboardImageBytes {
		t.Fatalf("exact-limit command result = (%d bytes, %v), want (%d bytes, true)", len(data), ok, maxClipboardImageBytes)
	}

	if err := os.WriteFile(payload, bytes.Repeat([]byte{'x'}, maxClipboardImageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	data, ok = runClipboardImageCommand(context.Background(), clipboardImageCommand{name: "clipboard-test-helper"})
	if ok || data != nil {
		t.Fatalf("over-limit command result = (%d bytes, %v), want (0 bytes, false)", len(data), ok)
	}
}

func writeClipboardTestHelper(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}
