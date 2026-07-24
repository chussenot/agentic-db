package tile

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSizeDirPixels(t *testing.T) {
	cases := map[string]int{
		"128x128":  128,
		"16x16":    16,
		"512x512":  512,
		"scalable": 0,
		"symbolic": 0,
		"":         0,
		"xbad":     0,
	}
	for in, want := range cases {
		if got := sizeDirPixels(in); got != want {
			t.Errorf("sizeDirPixels(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSanitizeIconName(t *testing.T) {
	// Reverse-DNS ids survive intact; path separators are neutralized.
	if got := sanitizeIconName("com.slack.Slack"); got != "com.slack.Slack" {
		t.Errorf("sanitizeIconName kept reverse-DNS wrong: %q", got)
	}
	if got := sanitizeIconName("weird/../id"); strings.ContainsAny(got, "/\\") {
		t.Errorf("sanitizeIconName left a separator: %q", got)
	}
}

// fakePNG builds bytes with a valid PNG signature + IHDR declaring w x h. The
// pixel data is junk — pngDimensions only reads the header and buildPNGWrapperSVG
// only base64-encodes the bytes, so this is enough to exercise both.
func fakePNG(w, h int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	b = append(b, 0, 0, 0, 13) // IHDR length
	b = append(b, []byte("IHDR")...)
	var wh [8]byte
	binary.BigEndian.PutUint32(wh[0:4], uint32(w))
	binary.BigEndian.PutUint32(wh[4:8], uint32(h))
	b = append(b, wh[:]...)
	return append(b, 0x08, 0x06, 0x00, 0x00, 0x00) // color/compression/etc.
}

func TestPngDimensions(t *testing.T) {
	w, h, err := pngDimensions(fakePNG(128, 96))
	if err != nil || w != 128 || h != 96 {
		t.Fatalf("pngDimensions = %d,%d,%v want 128,96,nil", w, h, err)
	}
	if _, _, err := pngDimensions([]byte("not a png at all!!!!!!!!")); err == nil {
		t.Error("pngDimensions accepted non-PNG bytes")
	}
	if _, _, err := pngDimensions(fakePNG(0, 10)); err == nil {
		t.Error("pngDimensions accepted zero width")
	}
}

func TestBuildPNGWrapperSVG(t *testing.T) {
	src := filepath.Join(t.TempDir(), "icon.png")
	raw := fakePNG(64, 64)
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	svg, err := buildPNGWrapperSVG(src)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	for _, want := range []string{
		`width="64"`, `height="64"`, `viewBox="0 0 64 64"`,
		`xlink:href="data:image/png;base64,`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapper SVG missing %q:\n%s", want, s)
		}
	}
	// The embedded payload must be the exact source bytes.
	enc := base64.StdEncoding.EncodeToString(raw)
	if !strings.Contains(s, enc) {
		t.Error("wrapper SVG did not embed the source PNG bytes verbatim")
	}
}

// TestWrappedPNGIconLive resolves a real PNG-only app if one is present on this
// box (firefox is the motivating case) and checks the generated wrapper is a
// cached, parseable SVG. Skips cleanly on a machine without such an icon.
func TestWrappedPNGIconLive(t *testing.T) {
	if findAppSVG("firefox") != "" {
		t.Skip("firefox ships a scalable SVG here; no PNG-wrap needed")
	}
	if findAppPNG("firefox") == "" {
		t.Skip("no firefox PNG on this box")
	}
	got := wrappedPNGIcon("firefox")
	if got == "" {
		t.Fatal("wrappedPNGIcon(firefox) returned empty")
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("wrapper not readable: %v", err)
	}
	if !strings.Contains(string(data), "data:image/png;base64,") {
		t.Errorf("wrapper %s is not an embedded-PNG SVG", got)
	}
}
