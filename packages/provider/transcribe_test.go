package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func transcriptionClient(srv *httptest.Server, account string) *ChatGPTTranscriber {
	return &ChatGPTTranscriber{Client: srv.Client(), BaseURL: srv.URL + "/backend-api", Credentials: func(context.Context) (string, string, error) { return "synthetic-token", account, nil }}
}
func transcriptionAudio(language string) AudioInput {
	return AudioInput{Filename: "audio.webm", MIMEType: "audio/webm", Reader: strings.NewReader("synthetic-audio"), Language: language}
}

func TestTranscribeMultipart(t *testing.T) {
	for _, language := range []string{"", "fr"} {
		t.Run("language="+language, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/backend-api/transcribe" {
					t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" || r.Header.Get("User-Agent") != "codex-cli" {
					t.Error("incorrect headers")
				}
				mr, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				part, err := mr.NextPart()
				if err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(part)
				if err != nil || part.FormName() != "file" || part.FileName() != "audio.webm" || part.Header.Get("Content-Type") != "audio/webm" || string(data) != "synthetic-audio" {
					t.Error("incorrect audio part")
				}
				part, err = mr.NextPart()
				if language != "" {
					if err != nil {
						t.Error(err)
						return
					}
					data, err = io.ReadAll(part)
					if err != nil || part.FormName() != "language" || string(data) != language {
						t.Error("incorrect language")
					}
					_, err = mr.NextPart()
				}
				if err != io.EOF {
					t.Errorf("unexpected extra part: %v", err)
				}
				fmt.Fprint(w, `{"text":"  hello world\n", "asset_ttl":"30d"}`)
			}))
			defer srv.Close()
			text, err := transcriptionClient(srv, "synthetic-account").Transcribe(context.Background(), transcriptionAudio(language))
			if err != nil || text != "hello world" {
				t.Fatalf("text=%q err=%v", text, err)
			}
		})
	}
}

func TestTranscribeResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		sentinel error
	}{
		{"unauthorized", 401, "private-response", ErrTranscribeUnauthorized},
		{"forbidden", 403, "private-response", ErrTranscribeUnauthorized},
		{"server", 500, "private-response", nil},
		{"empty", 200, `{"text":" \n "}`, ErrTranscribeEmpty},
		{"invalid", 200, "private-response", ErrTranscribeResponse},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), ErrTranscribeResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, present := r.Header["Chatgpt-Account-Id"]; present {
					t.Error("unexpected account header")
				}
				io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			_, err := transcriptionClient(srv, "").Transcribe(context.Background(), transcriptionAudio(""))
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Fatalf("error=%v", err)
			}
			if tc.status == 500 {
				var status *TranscribeHTTPError
				if !errors.As(err, &status) || status.StatusCode != 500 {
					t.Fatalf("missing typed status: %v", err)
				}
			}
			if tc.status != 200 && !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
				t.Fatalf("missing status: %v", err)
			}
			for _, secret := range []string{"private-response", "synthetic-token", "synthetic-audio"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("sensitive error")
				}
			}
		})
	}
}

func TestTranscribeCredentials(t *testing.T) {
	want := errors.New("credential source failed")
	client := &ChatGPTTranscriber{Credentials: func(context.Context) (string, string, error) { return "", "", want }}
	_, err := client.Transcribe(context.Background(), transcriptionAudio(""))
	if !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
}

func TestTranscribeCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := transcriptionClient(srv, "").Transcribe(ctx, transcriptionAudio("")); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation ignored")
	}
}

func TestTranscribeRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := transcriptionClient(srv, "synthetic-account").Transcribe(context.Background(), transcriptionAudio(""))
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("error=%v", err)
	}
}

type transcribeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f transcribeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type unreadTranscribeAudio struct{}

func (unreadTranscribeAudio) Read([]byte) (int, error) {
	panic("audio must not be read before the multipart header is consumed")
}

func TestTranscribeCancelBeforeBodyConsumed(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transcriber := &ChatGPTTranscriber{
		Credentials: func(context.Context) (string, string, error) { return "synthetic-token", "", nil },
		Client: &http.Client{Transport: transcribeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})},
	}
	returned := make(chan error, 1)
	go func() {
		_, err := transcriber.Transcribe(ctx, AudioInput{Filename: "audio.wav", MIMEType: "audio/wav", Reader: unreadTranscribeAudio{}})
		returned <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("transport not entered")
	}
	cancel()
	// Transcribe joins its multipart writer before returning, so this also proves
	// that the writer blocked on the unread header has exited.
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not release multipart writer")
	}
}

func TestTranscribeTransportErrors(t *testing.T) {
	for _, tc := range []struct{ cause, errorType error }{
		{errors.New("private transport details"), ErrTranscribeNetwork},
		{context.DeadlineExceeded, context.DeadlineExceeded},
	} {
		client := &ChatGPTTranscriber{Credentials: func(context.Context) (string, string, error) { return "synthetic-token", "", nil }, Client: &http.Client{Transport: transcribeRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tc.cause })}}
		_, err := client.Transcribe(context.Background(), transcriptionAudio(""))
		if !errors.Is(err, tc.errorType) || strings.Contains(err.Error(), "private") {
			t.Fatalf("%v", err)
		}
	}
}

type transcribeErrorReader struct{ err error }

func (r transcribeErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestTranscribeBodyReadTimeout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cause, want error
	}{
		{"timeout", os.ErrDeadlineExceeded, context.DeadlineExceeded},
		{"response", errors.New("private read details"), ErrTranscribeResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client := &ChatGPTTranscriber{
				Credentials: func(context.Context) (string, string, error) { return "synthetic-token", "", nil },
				Client: &http.Client{Transport: transcribeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					defer r.Body.Close()
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(transcribeErrorReader{tc.cause}), Header: make(http.Header)}, nil
				})},
			}
			_, err := client.Transcribe(ctx, transcriptionAudio(""))
			if !errors.Is(err, tc.want) || ctx.Err() != nil {
				t.Fatalf("error=%v context=%v", err, ctx.Err())
			}
		})
	}
}
