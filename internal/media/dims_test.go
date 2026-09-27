package media

import (
	"os"
	"path/filepath"
	"testing"
)

// thumbnailSOFJPEG builds a JPEG whose APP1 segment is 65535 bytes and starts
// with an SOF0 of 10 by 10. The real SOF0, 1920 by 1080, begins at offset
// 65539, past a 64 KiB scan. A marker walker returns 1920x1080. A raw search
// for FF C0 inside the first 64 KiB returns 10x10. The same bytes are posted
// by TestVideoJPEGThumbnailAspect.
func thumbnailSOFJPEG() []byte {
	// appLen includes the two length bytes, so the payload is 65533 bytes.
	const appLen = 65535
	payload := appLen - 2
	// decoy is an SOF0 marker sitting inside APP1, height 10 and width 10.
	decoy := []byte{0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x0a, 0x00, 0x0a, 0x01, 0x01, 0x11, 0x00}
	buf := make([]byte, 0, 2+4+payload+13+2)
	buf = append(buf, 0xff, 0xd8) // SOI
	buf = append(buf, 0xff, 0xe1) // APP1
	buf = append(buf, byte(appLen>>8), byte(appLen&0xff))
	buf = append(buf, decoy...)
	buf = append(buf, make([]byte, payload-len(decoy))...)
	// Real baseline SOF0: precision 8, height 1080 (0x0438), width 1920 (0x0780).
	buf = append(buf, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x04, 0x38, 0x07, 0x80, 0x01, 0x01, 0x11, 0x00)
	buf = append(buf, 0xff, 0xd9) // EOI
	return buf
}

// smallJPEG is a baseline JPEG with one short APP0 and an SOF0 of the given
// height and width. h and w must fit in 16 bits. A zero side is still written;
// ImageSize then reports ok false.
func smallJPEG(h, w int) []byte {
	return []byte{
		0xff, 0xd8,
		0xff, 0xe0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
		0xff, 0xc0, 0x00, 0x0b, 0x08,
		byte(h >> 8), byte(h),
		byte(w >> 8), byte(w),
		0x01, 0x01, 0x11, 0x00,
		0xff, 0xd9,
	}
}

// TestImageSizeSkipsThumbnailSOF reads the real frame past a 64 KiB APP1
// whose payload begins with a 10x10 SOF. A short APP0 JPEG still parses.
func TestImageSizeSkipsThumbnailSOF(t *testing.T) {
	dir := t.TempDir()
	jpg := thumbnailSOFJPEG()
	if len(jpg) <= 64<<10 {
		t.Fatalf("fixture length %d does not pass 64KiB", len(jpg))
	}
	// The real marker must sit past a 64 KiB window: SOI + APP1 marker + length + payload.
	if jpg[65539] != 0xff || jpg[65540] != 0xc0 {
		t.Fatalf("real SOF missing at 65539: %x", jpg[65539:65541])
	}
	big := filepath.Join(dir, "thumb.jpg")
	if err := os.WriteFile(big, jpg, 0o600); err != nil {
		t.Fatal(err)
	}
	w, h, ok := ImageSize(big)
	if !ok || w != 1920 || h != 1080 {
		t.Fatalf("ImageSize = %d %d ok %v", w, h, ok)
	}
	small := filepath.Join(dir, "small.jpg")
	if err := os.WriteFile(small, smallJPEG(8, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	w, h, ok = ImageSize(small)
	if !ok || w != 16 || h != 8 {
		t.Fatalf("small ImageSize = %d %d ok %v", w, h, ok)
	}
}
