// Package uploads keeps hosted upload content on disk (PROTOCOL.md §4.8.4):
// the directory that holds it, the bytes of bodies still arriving, which
// the server's storage bound counts, and the content types a hosted file
// is served as. It owns no lock: the count of arriving bytes is atomic, as
// bodies arrive without the server's lock. It knows nothing of embeds or
// messages; the server keeps which file is whose.
package uploads

import (
	"errors"
	"image"
	_ "image/gif" // Registered for image.DecodeConfig.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	_ "golang.org/x/image/webp" // Registered for image.DecodeConfig.
)

// Suffix names upload files, so stale ones can be told apart from anything
// else in the directory.
const Suffix = ".upload"

// ErrStore is the server's failure to keep an upload's file, rather than
// the writer's.
var ErrStore = errors.New("cannot store the upload")

// ErrArriving refuses the part of an upload's body that would take the
// bodies arriving at once past the storage bound.
var ErrArriving = errors.New("too many uploads arriving")

// Dir is the directory that holds upload content.
type Dir struct {
	path string
	// temp reports a temporary directory, removed by Close.
	temp bool
	// limit bounds the bytes arriving at once.
	limit int64
	// arriving counts the bytes written by uploads whose bodies are
	// arriving, or not yet recorded (Release).
	arriving atomic.Int64
}

// Open prepares the upload directory: dir, or a new temporary one when dir
// is empty. Bodies arriving at once may hold at most limit bytes.
func Open(dir string, limit int64) *Dir {
	d := &Dir{limit: limit}
	if dir == "" {
		temp, err := os.MkdirTemp("", "aprond-uploads-")
		if err != nil {
			slog.Error("cannot create a temporary upload directory", "error", err)
			return d
		}
		d.path, d.temp = temp, true
		return d
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("cannot create the upload directory", "dir", dir, "error", err)
	}
	d.path = dir
	return d
}

// Path is the file named name in the directory.
func (d *Dir) Path(name string) string { return filepath.Join(d.path, name) }

// Close removes a temporary directory.
func (d *Dir) Close() {
	if d.temp {
		_ = os.RemoveAll(d.path)
	}
}

// RemoveStale deletes upload files in the directory but those keep holds,
// such as those of a run without persistence.
func (d *Dir) RemoveStale(keep map[string]bool) {
	stale, _ := filepath.Glob(filepath.Join(d.path, "*"+Suffix))
	for _, name := range stale {
		if !keep[name] {
			_ = os.Remove(name)
		}
	}
}

// Arriving is the bytes arriving, or not yet released.
func (d *Dir) Arriving() int64 { return d.arriving.Load() }

// Release stops counting n bytes as arriving, once their upload is
// recorded, and so counted in the server's storage, or its file removed.
func (d *Dir) Release(n int64) { d.arriving.Add(-n) }

// Receive copies a request body of at most limit bytes into a new file in
// the directory, and returns the bytes written, which it counts as
// arriving until Release. On error the file, if any, is still returned so
// the caller can remove it.
func (d *Dir) Receive(w http.ResponseWriter, r *http.Request, limit int64) (*os.File, int64, error) {
	if limit <= 0 {
		return nil, 0, &http.MaxBytesError{Limit: limit}
	}
	file, err := os.CreateTemp(d.path, "*"+Suffix)
	if err != nil {
		slog.Error("cannot store upload", "error", err)
		return nil, 0, ErrStore
	}
	written := arrivingWriter{file: file, arriving: &d.arriving, limit: d.limit}
	size, err := io.Copy(written, http.MaxBytesReader(w, r.Body, limit))
	return file, size, err
}

// arrivingWriter writes an upload's body to its file, counting each byte
// written in arriving until the upload ends, and refuses what would take
// that count past limit. The hosted storage counts only finished uploads,
// so this bounds what is on disk while bodies arrive: the bytes that
// arrived, never a reservation for what may, so that bodies sent a byte at
// a time cannot hold the storage from everyone else.
type arrivingWriter struct {
	file     *os.File
	arriving *atomic.Int64
	limit    int64
}

func (w arrivingWriter) Write(p []byte) (int, error) {
	if w.arriving.Add(int64(len(p))) > w.limit {
		w.arriving.Add(-int64(len(p)))
		return 0, ErrArriving
	}
	n, err := w.file.Write(p)
	w.arriving.Add(int64(n - len(p)))
	if err != nil {
		// Writing the file is the server's failure, such as a full disk.
		slog.Error("cannot store upload", "error", err)
		return n, ErrStore
	}
	return n, nil
}

// ContentType prefers the sender's declared type, except for types a
// browser would run as a document, and otherwise sniffs the content.
func ContentType(declared string, head []byte) string {
	if mediaType, _, err := mime.ParseMediaType(declared); err == nil && mediaType != "application/octet-stream" {
		if !activeType(mediaType) {
			return mediaType
		}
	}
	mediaType, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	if activeType(mediaType) {
		return "application/octet-stream"
	}
	return mediaType
}

// activeType reports whether a browser could run or apply content of the
// type on this origin: markup, scripts, stylesheets, and WebAssembly.
func activeType(mediaType string) bool {
	for _, marker := range []string{"html", "xml", "xsl", "script", "jscript", "css", "wasm"} {
		if strings.Contains(mediaType, marker) {
			return true
		}
	}
	return false
}

// ValidImage checks that an avatar's or an emoji's content decodes as the
// image type it declares, one that ImageType takes.
func ValidImage(contentType string, file *os.File) bool {
	if !ImageType(contentType) || file == nil {
		return false
	}
	_, format, err := image.DecodeConfig(io.NewSectionReader(file, 0, 1<<30))
	return err == nil && "image/"+format == contentType
}

// ImageType reports whether a type is one avatars take (§4.8.6).
func ImageType(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Describe builds the og the server sets on a finished upload (§4.8.4),
// served at url: image for a preview, with alt, or video or audio to play,
// with title. Other files have none, so clients show a file card.
func Describe(url, contentType, title, alt string, file *os.File) map[string]any {
	media := map[string]any{"url": url, "type": contentType}
	og := map[string]any{}
	switch {
	case ImageType(contentType):
		if config, _, err := image.DecodeConfig(io.NewSectionReader(file, 0, 1<<30)); err == nil {
			media["width"], media["height"] = config.Width, config.Height
		}
		if alt != "" {
			media["alt"] = alt
		}
		og["image"] = media
	case strings.HasPrefix(contentType, "video/"):
		og["video"] = media
	case strings.HasPrefix(contentType, "audio/"):
		og["audio"] = media
	default:
		return nil
	}
	if title != "" {
		og["title"] = title
	}
	return og
}

// Inline reports whether a hosted file may be shown in the browser rather
// than downloaded.
func Inline(contentType string) bool {
	return ImageType(contentType) || contentType == "text/plain" ||
		strings.HasPrefix(contentType, "video/") || strings.HasPrefix(contentType, "audio/")
}
