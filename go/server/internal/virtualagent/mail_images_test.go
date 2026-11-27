package virtualagent

import "testing"

// mail_images_test.go — the image lines a mail's images add to the text of
// the virtual agent's user message (LLM-1040): what the logged prompt, the
// transcript and learning extraction keep of them.
func TestImageLines(t *testing.T) {
	if got := imageLines(nil); got != "" {
		t.Errorf("no images: got %q", got)
	}
	got := imageLines([]mailImage{{Filename: "wagons.png"}, {Filename: "cart.png"}})
	want := "\n\nAttached images, in order:\n[image 1: wagons.png]\n[image 2: cart.png]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
