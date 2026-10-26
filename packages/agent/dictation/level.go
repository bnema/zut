package dictation

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
)

var (
	ErrNoRecorder        = errors.New("no recorder found")
	ErrOutputPlaceholder = errors.New("recorder requires an {output} placeholder")
	ErrTooLittleAudio    = errors.New("recorder produced too little audio")
	ErrFinalize          = errors.New("recorder did not stop gracefully")
)

// RecorderError reports a failed recorder without exposing process error chains.
type RecorderError struct{ Recorder, Hint string }

func (e *RecorderError) Error() string { return "recording failed (" + e.Recorder + "): " + e.Hint }

// Recorder returns the selected executable name, available immediately after Start.
func (r *Recording) Recorder() string { return r.result.Recorder }

// Level returns a perceptual microphone level in [0,1], or zero when unknown.
func (r *Recording) Level() float64 { return math.Float64frombits(r.level.Load()) }

// sampleLevel is best-effort: overrides may use a different WAV layout. Never
// propagate meter errors into recording. Only the supervisor calls this method.
func (r *Recording) sampleLevel() {
	level := 0.0
	defer func() { r.level.Store(math.Float64bits(level)) }()
	f, err := os.Open(r.result.Path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	end := info.Size() &^ 1
	start := max(int64(44), end-3200)
	if end <= start {
		return
	}
	data := make([]byte, end-start)
	n, _ := f.ReadAt(data, start)
	var sum float64
	for j := 0; j+1 < n; j += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(data[j:]))) / 32768
		sum += v * v
	}
	if n < 2 || sum == 0 {
		return
	}
	rms := math.Sqrt(sum / float64(n/2))
	level = min(1, max(0, (20*math.Log10(rms)+60)/60))
}
