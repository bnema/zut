package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func testClipboardImage(marker string, data string) clipboardImageAttachment {
	return clipboardImageAttachment{
		Marker: marker,
		Image:  provider.ImageBlock{MimeType: "image/png", Data: []byte(data)},
	}
}

func TestRestoredQueuedImageDoesNotConsumeLiteralClipboardMarker(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	message := core.QueuedMessage{
		Text:   "keep literal [clipboard image #1]",
		Images: []provider.ImageBlock{{MimeType: "image/png", Data: []byte("png-1")}},
	}

	if !i.restoreQueuedMessageToEditor(message) {
		t.Fatal("queued message was not restored")
	}
	text, images := preparePromptWithClipboardImages(i.ed.Value(), i.clipboardImages)

	if text != message.Text {
		t.Fatalf("resubmitted text = %q, want %q", text, message.Text)
	}
	if len(images) != 1 || string(images[0].Data) != "png-1" {
		t.Fatalf("resubmitted images = %#v, want png-1", images)
	}
}

func TestPreparePromptWithClipboardImagesStripsPresentMarker(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("describe this [clipboard image #1] please", pending)

	if text != "describe this please" {
		t.Fatalf("text = %q, want %q", text, "describe this please")
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
	if string(images[0].Data) != "png-1" {
		t.Fatalf("image data = %q, want png-1", string(images[0].Data))
	}
}

func TestPreparePromptWithClipboardImagesAllowsImageOnlyPrompt(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("[clipboard image #1]", pending)

	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}

func TestPreparePromptWithClipboardImagesIgnoresDeletedMarker(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("describe this from memory", pending)

	if text != "describe this from memory" {
		t.Fatalf("text = %q, want unchanged", text)
	}
	if len(images) != 0 {
		t.Fatalf("len(images) = %d, want 0", len(images))
	}
}

func TestPreparePromptWithClipboardImagesHandlesMultipleImagesInPasteOrder(t *testing.T) {
	pending := []clipboardImageAttachment{
		testClipboardImage("[clipboard image #1]", "png-1"),
		testClipboardImage("[clipboard image #2]", "png-2"),
	}

	text, images := preparePromptWithClipboardImages("compare [clipboard image #2] with [clipboard image #1]", pending)

	if text != "compare with" {
		t.Fatalf("text = %q, want %q", text, "compare with")
	}
	if len(images) != 2 {
		t.Fatalf("len(images) = %d, want 2", len(images))
	}
	if string(images[0].Data) != "png-1" || string(images[1].Data) != "png-2" {
		t.Fatalf("images not attached in paste order: %q, %q", string(images[0].Data), string(images[1].Data))
	}
}

func TestPreparePromptWithClipboardImagesDuplicateMarkerAttachesOnce(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("[clipboard image #1] and again [clipboard image #1]", pending)

	if strings.Contains(text, "[clipboard image #1]") {
		t.Fatalf("text still contains marker: %q", text)
	}
	if text != "and again" {
		t.Fatalf("text = %q, want %q", text, "and again")
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}

func TestPreparePromptWithClipboardImagesNoPendingPreservesWhitespace(t *testing.T) {
	input := "please review:\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"

	text, images := preparePromptWithClipboardImages(input, nil)

	if text != input {
		t.Fatalf("text changed without pending images:\n got %q\nwant %q", text, input)
	}
	if images != nil {
		t.Fatalf("images = %#v, want nil", images)
	}
}

func TestPreparePromptWithClipboardImagesPreservesMultilinePrompt(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}
	input := "compare this image [clipboard image #1]\n\nwith this code:\n\treturn 1"
	want := "compare this image\n\nwith this code:\n\treturn 1"

	text, images := preparePromptWithClipboardImages(input, pending)

	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}
