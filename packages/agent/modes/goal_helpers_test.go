package modes

import (
	"testing"

	"github.com/mattn/go-runewidth"
)

func assertRowsFitWidth(t *testing.T, rows []string, width int) {
	t.Helper()
	for index, row := range rows {
		if got := runewidth.StringWidth(stripANSIBytes(row)); got > width {
			t.Errorf("row %d width = %d, want <= %d: %q", index, got, width, stripANSIBytes(row))
		}
	}
}

func findSettingsItem(items []settingsItem, key string) *settingsItem {
	for index := range items {
		if items[index].key == key {
			return &items[index]
		}
	}
	return nil
}
