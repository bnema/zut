package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	LlamaCPPProviderID       = "llama.cpp"
	defaultHuggingFaceURL    = "https://huggingface.co"
	llamaCPPRequestTimeout   = 15 * time.Second
	llamaCPPProgressPollTime = 250 * time.Millisecond
)

// LlamaCPPModel is one entry returned by a llama.cpp router's /models endpoint.
type LlamaCPPModel struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases,omitempty"`
	Source    string   `json:"source,omitempty"`
	CanRemove bool     `json:"can_remove,omitempty"`
	Status    struct {
		Value    string                   `json:"value"`
		Args     []string                 `json:"args,omitempty"`
		Failed   bool                     `json:"failed,omitempty"`
		ExitCode *int                     `json:"exit_code,omitempty"`
		Progress map[string]LlamaCPPBytes `json:"progress,omitempty"`
	} `json:"status"`
	Architecture struct {
		InputModalities []string `json:"input_modalities,omitempty"`
	} `json:"architecture,omitempty"`
	Meta struct {
		Context      int    `json:"n_ctx,omitempty"`
		TrainContext int    `json:"n_ctx_train,omitempty"`
		Size         int64  `json:"size,omitempty"`
		FileType     string `json:"ftype,omitempty"`
	} `json:"meta,omitempty"`
	// Progress is emitted at the model's top level by current llama.cpp
	// routers. Status.Progress supports versions that follow the documented
	// nested shape.
	Progress map[string]LlamaCPPBytes `json:"progress,omitempty"`
}

// LlamaCPPBytes describes byte progress for one downloaded file.
type LlamaCPPBytes struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// LlamaCPPProgress is a user-facing load or download progress update.
type LlamaCPPProgress struct {
	Message  string
	Ratio    float64
	HasRatio bool
	Detail   string
}

// LlamaCPPClient talks to the model-management API exposed by llama-server
// when it is running in router mode.
type LlamaCPPClient struct {
	ServerURL string
	APIKey    string
	HTTP      *http.Client
}

// NormalizeLlamaCPPURL validates a router URL and removes a trailing /v1,
// which belongs to the inference API rather than the management API.
func NormalizeLlamaCPPURL(value string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("parse llama.cpp server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("llama.cpp server URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("llama.cpp server URL is missing a host")
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	return strings.TrimRight(u.String(), "/"), nil
}

// LlamaCPPInferenceURL returns the OpenAI-compatible inference base URL.
func LlamaCPPInferenceURL(serverURL string) (string, error) {
	normalized, err := NormalizeLlamaCPPURL(serverURL)
	if err != nil {
		return "", err
	}
	return normalized + "/v1", nil
}

func NewLlamaCPPClient(serverURL, apiKey string) (*LlamaCPPClient, error) {
	normalized, err := NormalizeLlamaCPPURL(serverURL)
	if err != nil {
		return nil, err
	}
	return &LlamaCPPClient{ServerURL: normalized, APIKey: apiKey, HTTP: &http.Client{Timeout: llamaCPPRequestTimeout}}, nil
}

func (c *LlamaCPPClient) request(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.ServerURL+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &payload)
		if payload.Error.Message != "" {
			return errors.New(payload.Error.Message)
		}
		return fmt.Errorf("llama.cpp returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("parse llama.cpp response: %w", err)
		}
	}
	return nil
}

func (c *LlamaCPPClient) List(ctx context.Context, reload bool) ([]LlamaCPPModel, error) {
	path := "/models"
	if reload {
		path += "?reload=1"
	}
	var response struct {
		Data []LlamaCPPModel `json:"data"`
	}
	if err := c.request(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	for _, model := range response.Data {
		if model.ID == "" || model.Status.Value == "" {
			return nil, errors.New("server is not running in llama.cpp router mode")
		}
	}
	return response.Data, nil
}

func (c *LlamaCPPClient) modelAction(ctx context.Context, path, model string) error {
	body := strings.NewReader(`{"model":` + strconv.Quote(model) + `}`)
	return c.request(ctx, http.MethodPost, path, body, nil)
}

func (c *LlamaCPPClient) Load(ctx context.Context, model string) error {
	return c.modelAction(ctx, "/models/load", model)
}

func (c *LlamaCPPClient) Unload(ctx context.Context, model string) error {
	return c.modelAction(ctx, "/models/unload", model)
}

func (c *LlamaCPPClient) Download(ctx context.Context, model string) error {
	return c.modelAction(ctx, "/models", model)
}

// Remove deletes a router-managed cache model from disk. Models discovered
// from --models-dir or presets are not removable through the router API.
func (c *LlamaCPPClient) Remove(ctx context.Context, model string) error {
	path := "/models?" + url.Values{"model": {model}}.Encode()
	return c.request(ctx, http.MethodDelete, path, nil, nil)
}

type llamaCPPEvent struct {
	Model string          `json:"model"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

func (c *LlamaCPPClient) watch(ctx context.Context, events chan<- llamaCPPEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ServerURL+"/models/sse", nil)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+c.APIKey)
	}
	// http.Client.Timeout covers the complete response body lifetime, which is
	// appropriate for ordinary management requests but would terminate this
	// long-lived SSE stream. Preserve the configured transport and other client
	// behavior while letting ctx control the stream lifetime.
	streamClient := *c.HTTP
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp SSE returned HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event llamaCPPEvent
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil {
			select {
			case events <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return scanner.Err()
}

func loadProgress(data json.RawMessage) (LlamaCPPProgress, bool) {
	var payload struct {
		Progress struct {
			Stages  []string `json:"stages"`
			Current string   `json:"current"`
			Stage   string   `json:"stage"`
			Value   *float64 `json:"value"`
		} `json:"progress"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return LlamaCPPProgress{}, false
	}
	stage := payload.Progress.Current
	if stage == "" {
		stage = payload.Progress.Stage
	}
	if stage == "" && payload.Progress.Value == nil {
		return LlamaCPPProgress{}, false
	}
	progress := LlamaCPPProgress{Message: "Loading model"}
	if stage != "" {
		progress.Message = "Loading " + strings.ReplaceAll(stage, "_", " ")
	}
	if payload.Progress.Value != nil {
		ratio := max(0, min(1, *payload.Progress.Value))
		if stage != "" && len(payload.Progress.Stages) > 0 {
			for index, candidate := range payload.Progress.Stages {
				if candidate == stage {
					ratio = (float64(index) + ratio) / float64(len(payload.Progress.Stages))
					break
				}
			}
		}
		progress.Ratio, progress.HasRatio = ratio, true
	}
	return progress, true
}

func downloadProgress(files map[string]LlamaCPPBytes) (LlamaCPPProgress, bool) {
	var done, total int64
	for _, file := range files {
		done += file.Done
		total += file.Total
	}
	if total <= 0 {
		return LlamaCPPProgress{}, false
	}
	return LlamaCPPProgress{
		Message: "Downloading model",
		Ratio:   float64(done) / float64(total), HasRatio: true,
		Detail: FormatBytes(done) + " / " + FormatBytes(total),
	}, true
}

func (c *LlamaCPPClient) LoadAndWait(ctx context.Context, model string, update func(LlamaCPPProgress)) (LlamaCPPModel, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan llamaCPPEvent, 8)
	go func() { _ = c.watch(watchCtx, events) }()
	if err := c.Load(ctx, model); err != nil {
		return LlamaCPPModel{}, err
	}
	update(LlamaCPPProgress{Message: "Loading model"})
	ticker := time.NewTicker(llamaCPPProgressPollTime)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return LlamaCPPModel{}, ctx.Err()
		case event := <-events:
			if event.Model == model {
				if progress, ok := loadProgress(event.Data); ok {
					update(progress)
				}
			}
		case <-ticker.C:
			models, err := c.List(ctx, false)
			if err != nil {
				return LlamaCPPModel{}, err
			}
			for _, entry := range models {
				if entry.ID != model {
					continue
				}
				if entry.Status.Value == "loaded" || entry.Status.Value == "sleeping" {
					return entry, nil
				}
				if entry.Status.Failed {
					if entry.Status.ExitCode != nil {
						return LlamaCPPModel{}, fmt.Errorf("model exited with code %d", *entry.Status.ExitCode)
					}
					return LlamaCPPModel{}, errors.New("model failed to load")
				}
			}
		}
	}
}

func (c *LlamaCPPClient) UnloadAndWait(ctx context.Context, model string) error {
	if err := c.Unload(ctx, model); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		models, err := c.List(ctx, false)
		if err != nil {
			return err
		}
		found := false
		for _, entry := range models {
			if entry.ID == model {
				found = true
				if entry.Status.Value == "unloaded" {
					return nil
				}
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *LlamaCPPClient) DownloadAndWait(ctx context.Context, model string, update func(LlamaCPPProgress)) ([]LlamaCPPModel, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan llamaCPPEvent, 8)
	go func() { _ = c.watch(watchCtx, events) }()
	if err := c.Download(ctx, model); err != nil {
		return nil, err
	}
	update(LlamaCPPProgress{Message: "Downloading model"})
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	sawDownload := false
	polls := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event := <-events:
			if event.Model != model {
				continue
			}
			switch event.Event {
			case "download_progress":
				sawDownload = true
				var payload struct {
					Progress map[string]LlamaCPPBytes `json:"progress"`
				}
				if json.Unmarshal(event.Data, &payload) == nil {
					if progress, ok := downloadProgress(payload.Progress); ok {
						update(progress)
					}
				}
			case "download_failed":
				return nil, errors.New("model download failed")
			case "download_finished":
				return c.List(ctx, true)
			}
		case <-ticker.C:
			models, err := c.List(ctx, false)
			if err != nil {
				return nil, err
			}
			polls++
			for _, entry := range models {
				if entry.ID != model {
					continue
				}
				if entry.Status.Value == "downloading" {
					sawDownload = true
					files := entry.Status.Progress
					if len(files) == 0 {
						files = entry.Progress
					}
					if progress, ok := downloadProgress(files); ok {
						update(progress)
					}
				} else if sawDownload || polls >= 2 {
					return c.List(ctx, true)
				}
			}
		}
	}
}

// LlamaCPPModels converts loaded router entries into zut model metadata.
func LlamaCPPModels(models []LlamaCPPModel, serverURL string) []Model {
	baseURL, err := LlamaCPPInferenceURL(serverURL)
	if err != nil {
		return nil
	}
	var out []Model
	for _, entry := range models {
		if entry.Status.Value != "loaded" && entry.Status.Value != "sleeping" {
			continue
		}
		contextWindow := entry.Meta.Context
		if contextWindow <= 0 {
			contextWindow = entry.Meta.TrainContext
		}
		if contextWindow <= 0 {
			contextWindow = 128000
		}
		maxOutput := min(16384, contextWindow)
		out = append(out, Model{
			Provider: LlamaCPPProviderID, ID: entry.ID, DisplayName: entry.ID,
			ContextWindow: contextWindow, MaxOutput: maxOutput,
			BaseURL: baseURL, Source: "live",
		})
	}
	return out
}

// FormatBytes renders byte counts using binary units.
func FormatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(bytes) / 1024
	unit := units[0]
	for index := 1; index < len(units) && value >= 1024; index++ {
		value /= 1024
		unit = units[index]
	}
	if value >= 10 {
		return fmt.Sprintf("%.1f %s", value, unit)
	}
	return fmt.Sprintf("%.2f %s", value, unit)
}

// HuggingFaceModel is one GGUF repository search result.
type HuggingFaceModel struct {
	ID        string `json:"id"`
	Downloads int64  `json:"downloads"`
}

type HuggingFaceQuantization struct {
	Name    string
	Size    int64
	HasSize bool
}

type HuggingFaceModelDetails struct {
	ID            string
	Gated         string
	Quantizations []HuggingFaceQuantization
}

type HuggingFaceClient struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

func NewHuggingFaceClient(token string) *HuggingFaceClient {
	return &HuggingFaceClient{Token: token, BaseURL: defaultHuggingFaceURL, HTTP: &http.Client{Timeout: llamaCPPRequestTimeout}}
}

func (c *HuggingFaceClient) request(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			return errors.New("hugging face rate limit reached")
		}
		return fmt.Errorf("hugging face returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("parse Hugging Face response: %w", err)
	}
	return nil
}

func (c *HuggingFaceClient) Search(ctx context.Context, query string) ([]HuggingFaceModel, error) {
	params := url.Values{"search": {query}, "filter": {"gguf"}, "sort": {"downloads"}, "direction": {"-1"}, "limit": {"20"}}
	var models []HuggingFaceModel
	if err := c.request(ctx, "/api/models?"+params.Encode(), &models); err != nil {
		return nil, err
	}
	return models, nil
}

var quantizationPattern = regexp.MustCompile(`(?i)(?:^|[-_.])((?:UD-)?(?:IQ[0-9](?:_[A-Z0-9]+)+|Q[0-9](?:_[A-Z0-9]+)+|BF16|F16|F32|MXFP[0-9](?:_[A-Z0-9]+)*))$`)
var shardSuffixPattern = regexp.MustCompile(`-[0-9]{5}-of-[0-9]{5}$`)

func (c *HuggingFaceClient) Details(ctx context.Context, id string) (HuggingFaceModelDetails, error) {
	parts := strings.Split(id, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	encoded := strings.Join(parts, "/")
	var payload struct {
		ID       string          `json:"id"`
		Gated    json.RawMessage `json:"gated"`
		Siblings []struct {
			Filename string `json:"rfilename"`
			Size     *int64 `json:"size"`
		} `json:"siblings"`
	}
	if err := c.request(ctx, "/api/models/"+encoded+"?blobs=true", &payload); err != nil {
		return HuggingFaceModelDetails{}, err
	}
	type quantSize struct {
		total    int64
		complete bool
	}
	sizes := map[string]quantSize{}
	for _, file := range payload.Siblings {
		name := filepath.Base(file.Filename)
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") || strings.HasPrefix(strings.ToLower(name), "mmproj") {
			continue
		}
		stem := shardSuffixPattern.ReplaceAllString(name[:len(name)-5], "")
		match := quantizationPattern.FindStringSubmatch(stem)
		if len(match) < 2 {
			continue
		}
		quant := strings.ToUpper(match[1])
		current, ok := sizes[quant]
		if !ok {
			current.complete = true
		}
		if file.Size == nil {
			current.complete = false
		} else {
			current.total += *file.Size
		}
		sizes[quant] = current
	}
	var quantizations []HuggingFaceQuantization
	for name, size := range sizes {
		quantizations = append(quantizations, HuggingFaceQuantization{Name: name, Size: size.total, HasSize: size.complete})
	}
	sort.Slice(quantizations, func(i, j int) bool {
		if quantizations[i].Name == "Q4_K_M" {
			return true
		}
		if quantizations[j].Name == "Q4_K_M" {
			return false
		}
		if quantizations[i].HasSize != quantizations[j].HasSize {
			return quantizations[i].HasSize
		}
		if quantizations[i].Size != quantizations[j].Size {
			return quantizations[i].Size < quantizations[j].Size
		}
		return quantizations[i].Name < quantizations[j].Name
	})
	gated := ""
	_ = json.Unmarshal(payload.Gated, &gated)
	if payload.ID == "" {
		payload.ID = id
	}
	return HuggingFaceModelDetails{ID: payload.ID, Gated: gated, Quantizations: quantizations}, nil
}

// FindHuggingFaceToken follows the standard Hugging Face token locations.
func FindHuggingFaceToken() string {
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token != "" {
		return token
	}
	home, _ := os.UserHomeDir()
	paths := []string{os.Getenv("HF_TOKEN_PATH")}
	if value := os.Getenv("HF_HOME"); value != "" {
		paths = append(paths, filepath.Join(value, "token"))
	}
	if value := os.Getenv("XDG_CACHE_HOME"); value != "" {
		paths = append(paths, filepath.Join(value, "huggingface", "token"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".cache", "huggingface", "token"))
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				return token
			}
		}
	}
	return ""
}
