package dictation

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func TestLevelGrowingWAV(t *testing.T) {
	path := t.TempDir() + "/audio.wav"
	r := &Recording{result: Result{Path: path, Recorder: "synthetic"}}
	if r.Level() != 0 || r.Recorder() != "synthetic" {
		t.Fatal("initial state")
	}
	r.sampleLevel() // Missing file is harmless.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(make([]byte, 44+3200)); err != nil {
		t.Fatal(err)
	}
	r.sampleLevel()
	if r.Level() != 0 {
		t.Fatal(r.Level())
	}
	data := make([]byte, 3200)
	for j := 0; j < 1600; j++ {
		v := int16(28000 * math.Sin(2*math.Pi*440*float64(j)/16000))
		binary.LittleEndian.PutUint16(data[j*2:], uint16(v))
	}
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
	r.sampleLevel()
	if r.Level() < 0.85 || r.Level() > 1 {
		t.Fatal(r.Level())
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.sampleLevel()
	if r.Level() != 0 {
		t.Fatal(r.Level())
	}
}
