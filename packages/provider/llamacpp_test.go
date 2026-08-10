package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeLlamaCPPURL(t *testing.T) {
	got, err := NormalizeLlamaCPPURL("http://127.0.0.1:8080/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080" {
		t.Fatalf("got %q", got)
	}
	if _, err := NormalizeLlamaCPPURL("file:///tmp/router"); err == nil {
		t.Fatal("expected non-http URL to fail")
	}
}

func TestLlamaCPPLoadProgressStages(t *testing.T) {
	progress, ok := loadProgress(json.RawMessage(`{"progress":{"stages":["text_model","mmproj_model"],"current":"text_model","value":0.5}}`))
	if !ok || !progress.HasRatio || progress.Ratio != 0.25 || progress.Message != "Loading text model" {
		t.Fatalf("progress = %#v, ok = %v", progress, ok)
	}
}

func TestLlamaCPPWatchOutlivesRequestTimeout(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseEvent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestStarted)
		select {
		case <-releaseEvent:
			fmt.Fprint(w, "data: {\"model\":\"test\",\"event\":\"download_progress\",\"data\":{}}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	client, err := NewLlamaCPPClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan llamaCPPEvent, 1)
	watchDone := make(chan error, 1)
	go func() { watchDone <- client.watch(ctx, events) }()
	<-requestStarted

	select {
	case err := <-watchDone:
		t.Fatalf("watch stopped at ordinary request timeout: %v", err)
	case <-time.After(3 * client.HTTP.Timeout):
	}
	close(releaseEvent)
	select {
	case event := <-events:
		if event.Model != "test" || event.Event != "download_progress" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delayed SSE event")
	}
}

func TestLlamaCPPModelAcceptsTopLevelDownloadProgress(t *testing.T) {
	var model LlamaCPPModel
	if err := json.Unmarshal([]byte(`{"id":"test","status":{"value":"downloading"},"progress":{"model.gguf":{"done":512,"total":1024}}}`), &model); err != nil {
		t.Fatal(err)
	}
	progress, ok := downloadProgress(model.Progress)
	if !ok || progress.Ratio != 0.5 {
		t.Fatalf("progress = %#v, ok = %v", progress, ok)
	}
}

func TestLlamaCPPClientLoadAndDownloadProgress(t *testing.T) {
	var mu sync.Mutex
	status := "unloaded"
	events := make(chan string, 8)
	watchDone := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		switch {
		case r.URL.Path == "/models/sse":
			defer func() { watchDone <- struct{}{} }()
			w.Header().Set("content-type", "text/event-stream")
			flusher := w.(http.Flusher)
			for {
				select {
				case line := <-events:
					fmt.Fprintf(w, "data: %s\n\n", line)
					flusher.Flush()
					mu.Lock()
					if strings.Contains(line, `"event":"status_change"`) {
						status = "loaded"
					}
					if strings.Contains(line, `"event":"download_finished"`) {
						status = "unloaded"
					}
					mu.Unlock()
				case <-r.Context().Done():
					return
				}
			}
		case r.URL.Path == "/models/load":
			mu.Lock()
			status = "loading"
			mu.Unlock()
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"success":true}`)
			events <- `{"model":"test","event":"status_change","data":{"status":"loading","progress":{"stages":["text_model"],"current":"text_model","value":0.5}}}`
		case r.URL.Path == "/models" && r.Method == http.MethodPost:
			mu.Lock()
			status = "downloading"
			mu.Unlock()
			fmt.Fprint(w, `{"success":true}`)
			events <- `{"model":"test","event":"download_progress","data":{"progress":{"model.gguf":{"done":512,"total":1024}}}}`
			events <- `{"model":"test","event":"download_finished","data":{}}`
		case r.URL.Path == "/models":
			mu.Lock()
			current := status
			mu.Unlock()
			modelStatus := map[string]any{"value": current}
			if current == "downloading" {
				modelStatus["progress"] = map[string]any{"model.gguf": map[string]any{"done": 512, "total": 1024}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "test", "status": modelStatus}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewLlamaCPPClient(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var loadProgress []LlamaCPPProgress
	model, err := client.LoadAndWait(ctx, "test", func(progress LlamaCPPProgress) { loadProgress = append(loadProgress, progress) })
	if err != nil {
		t.Fatal(err)
	}
	if model.Status.Value != "loaded" {
		t.Fatalf("status = %q", model.Status.Value)
	}
	if len(loadProgress) == 0 {
		t.Fatal("expected load progress updates")
	}
	select {
	case <-watchDone:
	case <-ctx.Done():
		t.Fatal("load progress watcher did not stop")
	}
	var download []LlamaCPPProgress
	if _, err := client.DownloadAndWait(ctx, "test", func(progress LlamaCPPProgress) { download = append(download, progress) }); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, progress := range download {
		if progress.HasRatio && progress.Ratio == 0.5 && strings.Contains(progress.Detail, "512 B") {
			found = true
		}
	}
	if !found {
		t.Fatalf("download progress = %#v", download)
	}
}

func TestLlamaCPPClientRemoveUsesDeleteWithModelQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/models" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("model"); got != "owner/model:Q4_K_M" {
			t.Errorf("model = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer server.Close()

	client, err := NewLlamaCPPClient(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Remove(context.Background(), "owner/model:Q4_K_M"); err != nil {
		t.Fatal(err)
	}
}

func TestHuggingFaceDetailsCollectsGGUFQuantizations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			if r.URL.Query().Get("filter") != "gguf" || r.URL.Query().Get("search") != "coder" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[{"id":"owner/model-GGUF","downloads":1200}]`)
			return
		}
		if r.URL.Path == "/api/models/owner/model-GGUF" {
			fmt.Fprint(w, `{"id":"owner/model-GGUF","gated":"manual","siblings":[{"rfilename":"model-Q5_K_M.gguf","size":6000},{"rfilename":"model-Q4_K_M-00001-of-00002.gguf","size":2000},{"rfilename":"model-Q4_K_M-00002-of-00002.gguf","size":3000},{"rfilename":"mmproj-F16.gguf","size":1000}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewHuggingFaceClient("token")
	client.BaseURL = server.URL
	models, err := client.Search(context.Background(), "coder")
	if err != nil || len(models) != 1 {
		t.Fatalf("search: models=%#v err=%v", models, err)
	}
	details, err := client.Details(context.Background(), models[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Gated != "manual" || len(details.Quantizations) != 2 {
		t.Fatalf("details = %#v", details)
	}
	if details.Quantizations[0].Name != "Q4_K_M" || details.Quantizations[0].Size != 5000 {
		t.Fatalf("first quantization = %#v", details.Quantizations[0])
	}
}

func TestSetManagedModelsDoesNotReplaceDiscoveredCatalog(t *testing.T) {
	SetLiveModels([]Model{{Provider: "openrouter", ID: "vendor/live", Source: "live"}})
	SetManagedModels([]Model{{Provider: LlamaCPPProviderID, ID: "local", Source: "live"}})
	t.Cleanup(func() {
		SetLiveModels(nil)
		SetManagedModels(nil)
	})
	if _, err := FindModel("openrouter", "vendor/live"); err != nil {
		t.Fatal(err)
	}
	if _, err := FindModel(LlamaCPPProviderID, "local"); err != nil {
		t.Fatal(err)
	}
}
