package dictation

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDetection(t *testing.T) {
	for _, winner := range []string{"pw-record", "parecord", "ffmpeg", ""} {
		t.Run(winner, func(t *testing.T) {
			var tried []string
			argv, name, err := command(Options{LookPath: func(s string) (string, error) {
				tried = append(tried, s)
				if s == winner {
					return "/bin/" + s, nil
				}
				return "", errors.New("missing")
			}}, "/tmp/audio.wav", "linux")
			all := []string{"pw-record", "parecord", "ffmpeg"}
			if winner == "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !errors.Is(err, ErrNoRecorder) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || name != winner {
				t.Fatalf("%s %v", name, err)
			}
			index := 0
			for all[index] != winner {
				index++
			}
			if !reflect.DeepEqual(tried, all[:index+1]) {
				t.Fatal(tried)
			}
			want := map[string][]string{
				"pw-record": {"/bin/pw-record", "--rate", "16000", "--channels", "1", "--format", "s16", "--container", "wav", "/tmp/audio.wav"},
				"parecord":  {"/bin/parecord", "--rate=16000", "--channels=1", "--format=s16le", "--file-format=wav", "/tmp/audio.wav"},
				"ffmpeg":    {"/bin/ffmpeg", "-loglevel", "error", "-nostdin", "-f", "pulse", "-i", "default", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-y", "/tmp/audio.wav"},
			}[winner]
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("%q", argv)
			}
		})
	}
}

func TestOverride(t *testing.T) {
	original := []string{"custom", "prefix={output}", "; literal"}
	argv, name, err := command(Options{Command: original, LookPath: func(string) (string, error) { t.Fatal("lookup called"); return "", nil }}, "a b.wav", "windows")
	if err != nil || name != "custom" || argv[1] != "prefix=a b.wav" || original[1] != "prefix={output}" {
		t.Fatalf("%q %s %v", argv, name, err)
	}
	if _, _, err = command(Options{Command: []string{"custom"}}, "a", "linux"); err == nil {
		t.Fatal("missing placeholder accepted")
	}
}

func TestPlatformInputs(t *testing.T) {
	opts := Options{LookPath: func(s string) (string, error) {
		if s == "ffmpeg" {
			return s, nil
		}
		return "", errors.New("missing")
	}}
	argv, _, err := command(opts, "a.wav", "darwin")
	if err != nil || !strings.Contains(strings.Join(argv, " "), "-f avfoundation -i :0") {
		t.Fatalf("%q %v", argv, err)
	}
	if _, _, err = command(opts, "a.wav", "windows"); err == nil || !strings.Contains(err.Error(), "Windows microphone") {
		t.Fatal(err)
	}
}
