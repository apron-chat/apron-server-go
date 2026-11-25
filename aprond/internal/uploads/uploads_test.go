package uploads

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirCountsWhatArrives(t *testing.T) {
	d := Open("", 8)
	defer d.Close()
	receive := func(body string, limit int64) (*os.File, int64, error) {
		r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		return d.Receive(httptest.NewRecorder(), r, limit)
	}
	file, size, err := receive("12345", 100)
	if err != nil || size != 5 || d.Arriving() != 5 {
		t.Fatalf("size %d, arriving %d, %v", size, d.Arriving(), err)
	}
	file.Close()
	// Bodies arriving at once share the bound.
	if other, _, err := receive("6789", 100); !errors.Is(err, ErrArriving) {
		t.Fatalf("past the bound: %v", err)
	} else {
		other.Close()
	}
	d.Release(size)
	if _, _, err := receive("123456789", 4); err == nil {
		t.Fatal("a body past its limit was taken")
	}
	stale := d.Path("stale" + Suffix)
	_ = os.WriteFile(stale, nil, 0o600)
	d.RemoveStale(map[string]bool{file.Name(): true})
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a stale upload was kept")
	}
	if _, err := os.Stat(file.Name()); err != nil {
		t.Fatal("a kept upload was removed")
	}
	dir := filepath.Dir(file.Name())
	d.Close()
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the temporary directory outlived Close")
	}
}
