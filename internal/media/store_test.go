package media

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pngBytes is a 1x1 PNG the store and input tests can sniff.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStoreTTLAndCap deletes expired files and then the oldest past the cap.
func TestStoreTTLAndCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_000, 0)
	st, err := NewStore(dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	st.Clock = func() time.Time { return now }
	path := filepath.Join(dir, "src.png")
	if err := os.WriteFile(path, pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := st.PutFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.MaxBytes = first.Size
	now = now.Add(time.Second)
	if _, err := st.PutFile(path, 0); err != nil {
		t.Fatal(err)
	}
	st.Sweep()
	if st.Count() != 1 {
		t.Fatalf("cap count %d", st.Count())
	}
	if _, _, err := st.Open(first.Name); err == nil {
		t.Fatal("oldest file survived the cap")
	}
	now = now.Add(2 * time.Minute)
	st.Sweep()
	if st.Count() != 0 {
		t.Fatalf("ttl count %d", st.Count())
	}
	if _, err := os.Stat(filepath.Join(dir, first.Name)); !os.IsNotExist(err) {
		t.Fatal("expired file still on disk")
	}
}

// TestNameOKRejectsOddNames so a bad GET does not need the filesystem.
func TestNameOKRejectsOddNames(t *testing.T) {
	if NameOK("../etc/passwd") || NameOK("abc.png") || !NameOK("0123456789abcdef0123456789abcdef.png") {
		t.Fatal("name")
	}
}
