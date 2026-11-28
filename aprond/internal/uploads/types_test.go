package uploads

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"testing"
)

// An upload of a type a browser could run is served as text, or as a
// download when its content sniffs as markup.
func TestUploadTypesThatCouldRunAreServedAsDownloads(t *testing.T) {
	for declared, want := range map[string]string{
		"image/png":        "image/png",
		"application/pdf":  "application/pdf",
		"text/html":        "text/plain",
		"text/ecmascript":  "text/plain",
		"text/css":         "text/plain",
		"text/xsl":         "text/plain",
		"application/wasm": "text/plain",
	} {
		if got := ContentType(declared, []byte("hello")); got != want {
			t.Errorf("%s: served as %s, want %s", declared, got, want)
		}
	}
	if got := ContentType("text/css", []byte("<html><script>")); got != "application/octet-stream" {
		t.Errorf("sniffed markup served as %s", got)
	}
}

// An image is valid only as the type it was declared.
func TestAvatarUploadsMustBeImages(t *testing.T) {
	check := func(contentType string, data []byte) bool {
		file, err := os.CreateTemp(t.TempDir(), "avatar")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		_, _ = file.Write(data)
		return ValidImage(contentType, file)
	}
	if !check("image/png", testPNG(t)) {
		t.Fatal("a PNG was refused")
	}
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if !check("image/webp", webp) {
		t.Fatal("a WebP was refused")
	}
	if check("image/png", []byte("not an image")) || check("image/jpeg", testPNG(t)) || check("image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")) {
		t.Fatal("a mismatched avatar was accepted")
	}
}

// testPNG is a small PNG image.
func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
