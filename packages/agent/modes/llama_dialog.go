package modes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

type llamaDialogStep int

const (
	llamaClosed llamaDialogStep = iota
	llamaLoading
	llamaModels
	llamaConfirmUnload
	llamaConfirmRemove
	llamaSearch
	llamaModelDetails
	llamaQuantizations
	llamaProgress
	llamaError
)

// llamaDialog manages models exposed by a configured llama.cpp router. Network
// operations run outside the input loop and update this state under mu.
type llamaDialog struct {
	mu sync.Mutex

	step         llamaDialogStep
	client       *provider.LlamaCPPClient
	hf           *provider.HuggingFaceClient
	models       []provider.LlamaCPPModel
	results      []provider.HuggingFaceModel
	details      provider.HuggingFaceModelDetails
	cursor       int
	query        string
	selected     string
	message      string
	progress     provider.LlamaCPPProgress
	cancel       context.CancelFunc
	searchCancel context.CancelFunc
	searchSeq    uint64
	invalidate   func()
}

func newLlamaDialog() *llamaDialog { return &llamaDialog{} }

func (d *llamaDialog) Active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.step != llamaClosed
}

func (d *llamaDialog) Open(serverURL, apiKey string, invalidate func()) error {
	if strings.TrimSpace(serverURL) == "" {
		return fmt.Errorf("configure the llama.cpp router with /login first")
	}
	client, err := provider.NewLlamaCPPClient(serverURL, apiKey)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.step = llamaLoading
	d.client = client
	d.hf = provider.NewHuggingFaceClient(provider.FindHuggingFaceToken())
	d.cursor = 0
	d.query = ""
	d.message = "Connecting to " + client.ServerURL
	d.invalidate = invalidate
	d.mu.Unlock()
	go d.refresh(llamaModels, "")
	return nil
}

func (d *llamaDialog) Close() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	if d.searchCancel != nil {
		d.searchCancel()
		d.searchCancel = nil
	}
	d.step = llamaClosed
	d.mu.Unlock()
}

func (d *llamaDialog) notify() {
	d.mu.Lock()
	invalidate := d.invalidate
	d.mu.Unlock()
	if invalidate != nil {
		invalidate()
	}
}

func (d *llamaDialog) setError(err error) {
	d.mu.Lock()
	if d.step != llamaClosed {
		d.step = llamaError
		d.message = err.Error()
		d.cancel = nil
	}
	d.mu.Unlock()
	d.notify()
}

func (d *llamaDialog) refresh(next llamaDialogStep, message string) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	models, err := client.List(context.Background(), true)
	if err != nil {
		d.setError(err)
		return
	}
	sort.Slice(models, func(i, j int) bool {
		il := models[i].Status.Value == "loaded" || models[i].Status.Value == "sleeping"
		jl := models[j].Status.Value == "loaded" || models[j].Status.Value == "sleeping"
		if il != jl {
			return il
		}
		return models[i].ID < models[j].ID
	})
	provider.SetManagedModelsForProvider(provider.LlamaCPPProviderID, provider.LlamaCPPModels(models, client.ServerURL))
	d.mu.Lock()
	if d.step != llamaClosed {
		d.models = models
		d.cursor = min(d.cursor, len(models))
		d.step = next
		d.message = message
		d.cancel = nil
	}
	d.mu.Unlock()
	d.notify()
}

func (d *llamaDialog) beginProgress(title, model string, run func(context.Context, func(provider.LlamaCPPProgress)) error) {
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.step = llamaProgress
	d.selected = model
	d.message = title
	d.progress = provider.LlamaCPPProgress{Message: "Starting"}
	d.cancel = cancel
	d.mu.Unlock()
	d.notify()
	go func() {
		err := run(ctx, func(progress provider.LlamaCPPProgress) {
			d.mu.Lock()
			if d.step == llamaProgress {
				d.progress = progress
			}
			d.mu.Unlock()
			d.notify()
		})
		if err != nil {
			if ctx.Err() != nil {
				d.refresh(llamaModels, "operation cancelled")
				return
			}
			d.setError(err)
			return
		}
		d.refresh(llamaModels, title+" complete")
	}()
}

func (d *llamaDialog) load(model string) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	d.beginProgress("Loading local model", model, func(ctx context.Context, update func(provider.LlamaCPPProgress)) error {
		_, err := client.LoadAndWait(ctx, model, update)
		return err
	})
}

func (d *llamaDialog) unload(model string) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	d.beginProgress("Unloading model", model, func(ctx context.Context, update func(provider.LlamaCPPProgress)) error {
		update(provider.LlamaCPPProgress{Message: "Unloading model"})
		return client.UnloadAndWait(ctx, model)
	})
}

func (d *llamaDialog) download(model string) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	d.beginProgress("Downloading model", model, func(ctx context.Context, update func(provider.LlamaCPPProgress)) error {
		_, err := client.DownloadAndWait(ctx, model, update)
		return err
	})
}

func (d *llamaDialog) remove(model string) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	d.beginProgress("Removing model", model, func(ctx context.Context, update func(provider.LlamaCPPProgress)) error {
		update(provider.LlamaCPPProgress{Message: "Removing cached files"})
		return client.Remove(ctx, model)
	})
}

func llamaModelLoaded(model provider.LlamaCPPModel) bool {
	return model.Status.Value == "loaded" || model.Status.Value == "sleeping"
}

func (d *llamaDialog) scheduleSearchLocked() {
	if d.searchCancel != nil {
		d.searchCancel()
		d.searchCancel = nil
	}
	d.searchSeq++
	d.results = nil
	d.cursor = 0
	query := strings.TrimSpace(d.query)
	if len(query) < 2 {
		d.message = "type at least 2 characters"
		return
	}
	d.message = "searching Hugging Face..."
	ctx, cancel := context.WithCancel(context.Background())
	d.searchCancel = cancel
	seq := d.searchSeq
	hf := d.hf
	go func() {
		timer := time.NewTimer(400 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		results, err := hf.Search(ctx, query)
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		if d.step != llamaSearch || d.searchSeq != seq || strings.TrimSpace(d.query) != query {
			d.mu.Unlock()
			return
		}
		d.searchCancel = nil
		if err != nil {
			d.results = nil
			d.message = err.Error()
		} else {
			d.results = results
			d.message = ""
			if len(results) == 0 {
				d.message = "no GGUF models found"
			}
		}
		d.mu.Unlock()
		d.notify()
	}()
}

func (d *llamaDialog) HandleKey(k tui.Key) {
	d.mu.Lock()
	step := d.step
	if k.Kind == tui.KeyCtrlC {
		cancel := d.cancel
		searchCancel := d.searchCancel
		client := d.client
		model := d.selected
		d.step = llamaClosed
		d.mu.Unlock()
		if cancel != nil {
			cancel()
			go func() { _ = client.Unload(context.Background(), model) }()
		}
		if searchCancel != nil {
			searchCancel()
		}
		return
	}
	if k.Kind == tui.KeyEsc {
		switch step {
		case llamaProgress:
			cancel := d.cancel
			client := d.client
			model := d.selected
			d.mu.Unlock()
			if cancel != nil {
				cancel()
				go func() { _ = client.Unload(context.Background(), model) }()
			}
			return
		case llamaModelDetails, llamaQuantizations:
			d.step = llamaSearch
			d.cursor = 0
			d.mu.Unlock()
			return
		case llamaSearch:
			if d.searchCancel != nil {
				d.searchCancel()
				d.searchCancel = nil
			}
			d.step = llamaModels
			d.cursor = 0
			d.mu.Unlock()
			return
		case llamaConfirmUnload, llamaConfirmRemove:
			d.step = llamaModels
			d.cursor = 0
			d.mu.Unlock()
			return
		case llamaError:
			d.step = llamaClosed
			d.mu.Unlock()
			return
		default:
			d.step = llamaClosed
			d.mu.Unlock()
			return
		}
	}

	switch step {
	case llamaModels:
		count := len(d.models) + 1
		switch k.Kind {
		case tui.KeyUp:
			if d.cursor > 0 {
				d.cursor--
			}
		case tui.KeyDown:
			if d.cursor+1 < count {
				d.cursor++
			}
		case tui.KeyEnter:
			if d.cursor == len(d.models) {
				d.step, d.cursor, d.query, d.message = llamaSearch, 0, "", "type at least 2 characters"
				d.results = nil
				d.mu.Unlock()
				return
			}
			if len(d.models) > 0 {
				model := d.models[d.cursor]
				if llamaModelLoaded(model) {
					d.selected = model.ID
					d.step = llamaConfirmUnload
					d.cursor = 0
					d.mu.Unlock()
					return
				}
				if model.Status.Value == "unloaded" {
					id := model.ID
					d.mu.Unlock()
					d.load(id)
					return
				}
				d.message = model.ID + " is " + model.Status.Value
			}
		case tui.KeyRune:
			if (k.Rune == 'd' || k.Rune == 'D') && !k.Ctrl && !k.Alt && d.cursor < len(d.models) {
				model := d.models[d.cursor]
				if !model.CanRemove {
					d.message = "only router-downloaded cache models can be removed"
					break
				}
				d.selected = model.ID
				d.step = llamaConfirmRemove
				d.cursor = 0
			}
		}
	case llamaConfirmUnload:
		switch k.Kind {
		case tui.KeyUp, tui.KeyDown:
			d.cursor = 1 - d.cursor
		case tui.KeyEnter:
			if d.cursor == 0 {
				id := d.selected
				d.mu.Unlock()
				d.unload(id)
				return
			}
			d.step = llamaModels
		}
	case llamaConfirmRemove:
		switch k.Kind {
		case tui.KeyUp, tui.KeyDown:
			d.cursor = 1 - d.cursor
		case tui.KeyEnter:
			if d.cursor == 0 {
				id := d.selected
				d.mu.Unlock()
				d.remove(id)
				return
			}
			d.step = llamaModels
		}
	case llamaSearch:
		switch k.Kind {
		case tui.KeyBackspace:
			runes := []rune(d.query)
			if len(runes) > 0 {
				d.query = string(runes[:len(runes)-1])
				d.scheduleSearchLocked()
			}
		case tui.KeyPaste:
			d.query += strings.Join(strings.Fields(k.Paste), " ")
			d.scheduleSearchLocked()
		case tui.KeyRune:
			if !k.Ctrl && !k.Alt && k.Rune >= 0x20 && k.Rune < 0x7f {
				d.query += string(k.Rune)
				d.scheduleSearchLocked()
			}
		case tui.KeyUp:
			if d.cursor > 0 {
				d.cursor--
			}
		case tui.KeyDown:
			if d.cursor+1 < len(d.results) {
				d.cursor++
			}
		case tui.KeyEnter:
			if len(d.results) == 0 {
				break
			}
			if d.searchCancel != nil {
				d.searchCancel()
				d.searchCancel = nil
			}
			id := d.results[d.cursor].ID
			hf := d.hf
			d.step, d.message = llamaLoading, "loading model details"
			d.mu.Unlock()
			go func() {
				details, err := hf.Details(context.Background(), id)
				if err != nil {
					d.setError(err)
					return
				}
				d.mu.Lock()
				if d.step != llamaClosed {
					d.details, d.cursor = details, 0
					if len(details.Quantizations) > 0 {
						d.step = llamaQuantizations
					} else {
						d.step = llamaModelDetails
					}
				}
				d.mu.Unlock()
				d.notify()
			}()
			return
		}
	case llamaModelDetails:
		if k.Kind == tui.KeyEnter {
			id := d.details.ID
			d.mu.Unlock()
			d.download(id)
			return
		}
	case llamaQuantizations:
		switch k.Kind {
		case tui.KeyUp:
			if d.cursor > 0 {
				d.cursor--
			}
		case tui.KeyDown:
			if d.cursor+1 < len(d.details.Quantizations) {
				d.cursor++
			}
		case tui.KeyEnter:
			if len(d.details.Quantizations) > 0 {
				id := d.details.ID + ":" + d.details.Quantizations[d.cursor].Name
				d.mu.Unlock()
				d.download(id)
				return
			}
		}
	}
	d.mu.Unlock()
}

func (d *llamaDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.step == llamaClosed {
		return nil
	}
	lines := []string{frameHeader(th, "llama.cpp models", width)}
	if d.client != nil {
		lines = append(lines, th.FGColor(th.Muted, d.client.ServerURL))
	}
	if d.message != "" {
		lines = append(lines, th.FGColor(th.Muted, d.message))
	}

	switch d.step {
	case llamaLoading:
		lines = append(lines, th.FGColor(th.Muted, "  working..."))
	case llamaModels:
		const visible = 12
		start, end := dialogWindow(d.cursor, len(d.models)+1, visible)
		for index := start; index < end; index++ {
			label := "Download model from Hugging Face..."
			if index < len(d.models) {
				model := d.models[index]
				status := model.Status.Value
				if status == "loaded" || status == "sleeping" {
					status = "loaded"
				}
				label = fmt.Sprintf("%-10s  %s", status, model.ID)
			}
			line := "  " + label
			if index == d.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, th.FGColor(th.Muted, line))
			}
		}
		lines = append(lines, th.FGColor(th.Muted, "enter loads/unloads, d removes cached model, esc closes"))
	case llamaConfirmUnload:
		lines = append(lines, "", "  Unload "+d.selected+"?")
		for index, label := range []string{"Yes", "No"} {
			line := "  " + label
			if index == d.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, th.FGColor(th.Muted, line))
			}
		}
	case llamaConfirmRemove:
		lines = append(lines, "", "  Remove cached model "+d.selected+"?")
		for index, label := range []string{"Yes", "No"} {
			line := "  " + label
			if index == d.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, th.FGColor(th.Muted, line))
			}
		}
	case llamaSearch:
		lines = append(lines, "", th.FGColor(th.Muted, "  Model name or owner/repository"), th.FGColor(th.FG, "  "+d.query), "")
		start, end := dialogWindow(d.cursor, len(d.results), 10)
		for index := start; index < end; index++ {
			result := d.results[index]
			line := fmt.Sprintf("  %s  %s downloads", result.ID, compactNumber(result.Downloads))
			if index == d.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, th.FGColor(th.Muted, line))
			}
		}
		lines = append(lines, th.FGColor(th.Muted, "enter selects, esc goes back"))
	case llamaModelDetails:
		lines = append(lines, "", "  "+d.details.ID)
		if d.details.Gated != "" {
			lines = append(lines, th.FGColor(th.Error, "  access approval and HF_TOKEN may be required"))
		}
		lines = append(lines, th.FGColor(th.Muted, "  enter downloads, esc goes back"))
	case llamaQuantizations:
		lines = append(lines, "", "  Select quantization for "+d.details.ID)
		if d.details.Gated != "" {
			lines = append(lines, th.FGColor(th.Error, "  gated model: approve access on huggingface.co and configure HF_TOKEN on the router"))
		}
		start, end := dialogWindow(d.cursor, len(d.details.Quantizations), 10)
		for index := start; index < end; index++ {
			quant := d.details.Quantizations[index]
			detail := ""
			if quant.HasSize {
				detail = "  " + provider.FormatBytes(quant.Size)
			}
			if quant.Name == "Q4_K_M" {
				detail += "  recommended"
			}
			line := "  " + quant.Name + detail
			if index == d.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, th.FGColor(th.Muted, line))
			}
		}
	case llamaProgress:
		lines = append(lines, "", "  "+d.selected, th.FGColor(th.Muted, "  "+d.progress.Message))
		if d.progress.HasRatio {
			const cells = 32
			filled := int(d.progress.Ratio*float64(cells) + .5)
			filled = max(0, min(cells, filled))
			bar := strings.Repeat("#", filled) + strings.Repeat("-", cells-filled)
			lines = append(lines, th.FGColor(th.Accent, fmt.Sprintf("  %s %d%%", bar, int(d.progress.Ratio*100+.5))))
		}
		if d.progress.Detail != "" {
			lines = append(lines, th.FGColor(th.Muted, "  "+d.progress.Detail))
		}
		lines = append(lines, th.FGColor(th.Muted, "  esc stops"))
	case llamaError:
		lines = append(lines, th.FGColor(th.Error, "  "+d.message), th.FGColor(th.Muted, "  esc closes"))
	}
	return append(lines, frameRule(th, width))
}

// CursorPos returns the search input caret location inside the padded dialog.
func (d *llamaDialog) CursorPos() (row, col int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.step != llamaSearch {
		return -1, -1
	}
	row = 5 // header, injected frame padding, server URL, blank, prompt
	if d.message != "" {
		row++
	}
	return row, 2 + runewidth.StringWidth(d.query)
}

func dialogWindow(cursor, count, visible int) (int, int) {
	if count <= visible {
		return 0, count
	}
	start := cursor - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > count {
		start = count - visible
	}
	return start, start + visible
}

func compactNumber(value int64) string {
	if value >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	}
	if value >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	}
	return fmt.Sprintf("%d", value)
}
