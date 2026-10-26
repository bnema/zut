package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// ErrTranscribeUnauthorized indicates that the caller should refresh credentials or log in again.
var ErrTranscribeUnauthorized = errors.New("transcribe: unauthorized")

// ErrTranscribeEmpty indicates that no speech text was returned.
var ErrTranscribeEmpty = errors.New("transcribe: empty text")

// ErrTranscribeNetwork indicates transport failure without exposing request details.
var ErrTranscribeNetwork = errors.New("transcription network error")

// ErrTranscribeResponse indicates an unreadable or malformed response.
var ErrTranscribeResponse = errors.New("unexpected transcription response")

// TranscribeHTTPError contains only the HTTP status, never a response body.
type TranscribeHTTPError struct{ StatusCode int }

func (e *TranscribeHTTPError) Error() string { return fmt.Sprintf("transcribe: HTTP %d", e.StatusCode) }

// AudioInput describes an audio upload. Reader must return promptly (or be
// independently cancelable); Transcribe does not close caller-owned readers.
type AudioInput struct {
	Filename string
	MIMEType string
	Reader   io.Reader
	Language string
}

// Transcriber converts audio into text without exposing provider wire formats.
type Transcriber interface {
	Transcribe(ctx context.Context, audio AudioInput) (string, error)
}

// ChatGPTTranscriber uses ChatGPT's private, undocumented dictation endpoint.
// OpenAI retains uploaded audio (the endpoint reports asset_ttl of 30d).
// The caller owns credential refresh and should supply a bounded context.
type ChatGPTTranscriber struct {
	Client      *http.Client
	BaseURL     string
	Credentials func(ctx context.Context) (accessToken, accountID string, err error)
}

var _ Transcriber = (*ChatGPTTranscriber)(nil)

// Transcribe streams audio to ChatGPT and returns trimmed text. Credential-source
// errors are propagated unchanged; sources must not include secrets in errors.
func (t *ChatGPTTranscriber) Transcribe(ctx context.Context, audio AudioInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t.Credentials == nil {
		return "", errors.New("transcribe: missing credential source")
	}
	token, accountID, err := t.Credentials(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("transcribe: missing access token")
	}
	if audio.Reader == nil || audio.Filename == "" || audio.MIMEType == "" {
		return "", errors.New("transcribe: missing audio input")
	}
	if strings.ContainsAny(audio.MIMEType, "\r\n") {
		return "", errors.New("transcribe: invalid audio MIME type")
	}
	base := t.BaseURL
	if base == "" {
		base = "https://chatgpt.com/backend-api"
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	multipartWriter := multipart.NewWriter(writer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/transcribe", reader)
	if err != nil {
		return "", errors.New("transcribe: invalid endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("User-Agent", "codex-cli")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	// Closing the pipe interrupts blocked writes on cancellation, including when
	// the HTTP transport has not yet started consuming the request body.
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	defer stop()
	writerDone := make(chan struct{})
	defer func() { _ = reader.Close(); <-writerDone }()
	go func() {
		defer close(writerDone)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": audio.Filename}))
		header.Set("Content-Type", audio.MIMEType)
		part, err := multipartWriter.CreatePart(header)
		if err == nil {
			_, err = io.Copy(part, audio.Reader)
		}
		if err == nil && audio.Language != "" {
			err = multipartWriter.WriteField("language", audio.Language)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		writer.CloseWithError(err)
	}()
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return "", context.DeadlineExceeded
		}
		return "", ErrTranscribeNetwork
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: HTTP %d", ErrTranscribeUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &TranscribeHTTPError{StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || len(body) > 1<<20 {
		return "", ErrTranscribeResponse
	}
	var payload struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return "", ErrTranscribeResponse
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		return "", ErrTranscribeEmpty
	}
	return text, nil
}
