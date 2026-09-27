package media

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
)

// ImageSize reads the pixel size of a png or jpeg file.
// ok is false for webp, a short file, or a parse failure. Callers then keep
// the default video aspect ratio instead of guessing.
func ImageSize(path string) (w, h int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var hdr [32]byte
	n, _ := f.Read(hdr[:])
	if n >= 24 && string(hdr[1:4]) == "PNG" {
		w = int(binary.BigEndian.Uint32(hdr[16:20]))
		h = int(binary.BigEndian.Uint32(hdr[20:24]))
		return w, h, w > 0 && h > 0
	}
	if n < 4 || hdr[0] != 0xff || hdr[1] != 0xd8 {
		return 0, 0, false
	}
	return jpegSize(f)
}

// jpegSize reads the frame size by walking JPEG marker segments from the start.
// f may be positioned anywhere; this seeks to byte 0. Bytes inside a segment
// (an EXIF thumbnail's SOF, for example) are not markers: each segment's length
// is skipped, including segments longer than 64 KiB. The first top-level SOF0,
// SOF1, or SOF2 wins. A file with no such marker before the scan or EOI returns
// ok false, and the caller keeps the default video aspect.
func jpegSize(f *os.File) (int, int, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, false
	}
	br := bufio.NewReader(f)
	var magic [2]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || magic[0] != 0xff || magic[1] != 0xd8 {
		return 0, 0, false
	}
	for {
		marker, err := jpegMarker(br)
		if err != nil {
			return 0, 0, false
		}
		// Standalone markers have no length. EOI and the entropy scan end the header.
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if marker == 0xd9 || marker == 0xda {
			return 0, 0, false
		}
		var lenb [2]byte
		if _, err := io.ReadFull(br, lenb[:]); err != nil {
			return 0, 0, false
		}
		segLen := int(lenb[0])<<8 | int(lenb[1])
		if segLen < 2 {
			return 0, 0, false
		}
		if marker == 0xc0 || marker == 0xc1 || marker == 0xc2 {
			// precision + height + width is 5 bytes, and the length word counts itself.
			// A shorter segment is truncated, so the caller keeps the default aspect.
			if segLen < 7 {
				return 0, 0, false
			}
			var sof [5]byte
			if _, err := io.ReadFull(br, sof[:]); err != nil {
				return 0, 0, false
			}
			h := int(sof[1])<<8 | int(sof[2])
			w := int(sof[3])<<8 | int(sof[4])
			return w, h, w > 0 && h > 0
		}
		if _, err := io.CopyN(io.Discard, br, int64(segLen-2)); err != nil {
			return 0, 0, false
		}
	}
}

// jpegMarker reads the next marker byte, skipping the leading 0xFF fill.
// A truncated stream returns the read error. The caller uses the marker to
// decide whether a length word follows.
func jpegMarker(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xff {
			continue
		}
		for {
			b, err = r.ReadByte()
			if err != nil {
				return 0, err
			}
			if b != 0xff {
				return b, nil
			}
		}
	}
}

// ClosestAspect picks the video ratio nearest to w/h.
// The choices are the seven ratios reference_to_video accepts.
// A zero side returns 16:9.
func ClosestAspect(w, h int) string {
	choices := []struct {
		name string
		r    float64
	}{
		{"1:1", 1},
		{"16:9", 16.0 / 9},
		{"9:16", 9.0 / 16},
		{"4:3", 4.0 / 3},
		{"3:4", 3.0 / 4},
		{"3:2", 3.0 / 2},
		{"2:3", 2.0 / 3},
	}
	if w <= 0 || h <= 0 {
		return "16:9"
	}
	r := float64(w) / float64(h)
	best := choices[0].name
	diff := abs(r - choices[0].r)
	for _, c := range choices[1:] {
		d := abs(r - c.r)
		if d < diff {
			best = c.name
			diff = d
		}
	}
	return best
}

// abs is the absolute value of a ratio difference.
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
