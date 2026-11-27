package config

import (
	"fmt"
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
