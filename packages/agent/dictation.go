package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/bnema/zut/packages/agent/dictation"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/provider"
)

// DictationSettings configures interactive audio capture, independently of the
// selected model. Omitted max_seconds defaults to ten minutes.
type DictationSettings struct {
	Recorder   []string `json:"recorder,omitempty"`
	MaxSeconds *int     `json:"max_seconds,omitempty"`
	Language   string   `json:"language,omitempty"`
}

func validateDictationSettings(s *DictationSettings) error {
	if s != nil && s.MaxSeconds != nil && (*s.MaxSeconds <= 0 || *s.MaxSeconds > 1800) {
		return fmt.Errorf("dictation.max_seconds must be between 1 and 1800")
	}
	return nil
}

// dictationCredentials reads the stored ChatGPT OAuth login directly: the
// transcription endpoint rejects API keys, so env API keys must not shadow it.
func dictationCredentials(ctx context.Context) (string, string, error) {
	tok := loadOAuthToken("openai")
	if tok == nil || tok.AccessToken == "" {
		return "", "", modes.ErrDictationLogin
	}
	// A failed refresh returns the stale token; the endpoint then answers 401,
	// which the UI reports as an expired login.
	tok, _ = refreshIfExpiredContext(ctx, "openai", tok)
	return tok.AccessToken, tok.AccountID, nil
}

func interactiveDictationConfig(s *DictationSettings) modes.DictationConfig {
	cfg := modes.DictationConfig{
		Service:     "ChatGPT",
		Transcriber: &provider.ChatGPTTranscriber{Credentials: dictationCredentials},
		Recorder:    dictation.Options{MaxDuration: 600 * time.Second},
	}
	if s != nil {
		cfg.Recorder.Command = append([]string(nil), s.Recorder...)
		cfg.Language = s.Language
		if s.MaxSeconds != nil && *s.MaxSeconds > 0 && *s.MaxSeconds <= 1800 {
			cfg.Recorder.MaxDuration = time.Duration(*s.MaxSeconds) * time.Second
		}
	}
	return cfg
}
