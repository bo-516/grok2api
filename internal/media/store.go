package media

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// StoreCap is the total size of the media directory. Past this, Sweep deletes
// the oldest files until the sum is back under the cap. Tests set a smaller cap.
const StoreCap = 2 << 30

// maxImage and maxVideo are the largest files PutFile will accept.
// A larger file is refused and nothing is renamed into the store.
const (
	maxImage = 50 << 20
	maxVideo = 500 << 20
)

// namePat is the only filename GET /v1/media may open.
// Anything else is a 404 and must not touch the disk. The 32 hex digits are
// 128 bits from crypto/rand.
var namePat = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp|mp4)$`)

// NameOK reports whether name is a stored media filename.
// A false result means the caller returns 404 without Stat.
func NameOK(name string) bool { return namePat.MatchString(name) }

// Item is one stored file. Path is absolute. Name is the URL's last segment.
type Item struct {
	// Name is the 32-hex filename including the sniffed extension.
	Name string
	// Path is the absolute file path. It is mode 0600.
	Path string
	// Type is the Content-Type, such as image/jpeg or video/mp4.
	Type string
	// Size is the file size in bytes.
	Size int64
	// Created is when the file was stored. Sweep uses it for TTL and age.
	Created time.Time
}

// Store is the media directory. Files live only in memory's index plus the
// directory; a process restart does not reload them, so old names 404.
type Store struct {
	// Dir is the directory. It is created at 0700.
	Dir string
	// TTL is how long a file stays. Zero means it never expires by age.
	TTL time.Duration
	// MaxBytes is the capacity. Zero means StoreCap.
	MaxBytes int64
	// Clock is the time source. Nil uses time.Now. Tests advance it.
	Clock func() time.Time

	mu    sync.Mutex
	items map[string]*Item
}

// NewStore creates Dir at 0700 and an empty index.
// A mkdir failure returns the error and a nil store; callers must not serve media.
func NewStore(dir string, ttl time.Duration) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, TTL: ttl, MaxBytes: StoreCap, items: map[string]*Item{}}, nil
}

// now returns Clock or time.Now. A nil Clock is production time.
func (s *Store) now() time.Time {
	if s != nil && s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// capBytes returns the capacity. Zero MaxBytes means the 2 GiB default.
func (s *Store) capBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return StoreCap
}

// PutFile copies src into the store under a random name.
// src must be a regular file, not a symlink, and no larger than limit.
// limit <= 0 uses the image cap. The copy is 0600 and replaced atomically
// by rename. A failure leaves the store unchanged.
func (s *Store) PutFile(src string, limit int64) (Item, error) {
	if limit <= 0 {
		limit = maxImage
	}
	st, err := os.Lstat(src)
	if err != nil {
		return Item{}, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return Item{}, errors.New("media output is not a regular file")
	}
	if st.Size() > limit {
		return Item{}, errors.New("media output exceeds the size limit")
	}
	f, err := os.Open(src)
	if err != nil {
		return Item{}, err
	}
	defer f.Close()
	prefix := make([]byte, 512)
	n, _ := io.ReadFull(f, prefix)
	prefix = prefix[:max(n, 0)]
	ext, ctype, ok := sniff(prefix)
	if !ok {
		return Item{}, errors.New("media output is not png, jpeg, webp, or mp4")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Item{}, err
	}
	return s.write(ext, ctype, f, st.Size())
}

// PutBytes stores an inline image. b is sniffed and must be an image, not video,
// and at most the image cap. A bad sniff or a write error stores nothing.
func (s *Store) PutBytes(b []byte) (Item, error) {
	if int64(len(b)) > maxImage {
		return Item{}, errors.New("inline image exceeds the size limit")
	}
	ext, ctype, ok := sniff(b)
	if !ok || ext == "mp4" {
		return Item{}, errors.New("inline media is not png, jpeg, or webp")
	}
	return s.write(ext, ctype, bytes.NewReader(b), int64(len(b)))
}

// write creates name.tmp at 0600, copies r, and renames it into place.
// size is the expected length and is stored on the item. A short copy is an error
// and the temp file is removed.
func (s *Store) write(ext, ctype string, r io.Reader, size int64) (Item, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return Item{}, err
	}
	name := hex.EncodeToString(buf[:]) + "." + ext
	final := filepath.Join(s.Dir, name)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Item{}, err
	}
	n, werr := io.Copy(f, r)
	cerr := f.Close()
	if werr != nil || cerr != nil || n != size {
		os.Remove(tmp)
		if werr != nil {
			return Item{}, werr
		}
		if cerr != nil {
			return Item{}, cerr
		}
		return Item{}, errors.New("media copy was short")
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return Item{}, err
	}
	item := Item{Name: name, Path: final, Type: ctype, Size: size, Created: s.now()}
	s.mu.Lock()
	if s.items == nil {
		s.items = map[string]*Item{}
	}
	cp := item
	s.items[name] = &cp
	s.mu.Unlock()
	return item, nil
}

// Open opens name if it is in the index and the name matches NameOK.
// The caller closes the file. A bad or unknown name returns an error and
// does not create a path outside Dir. Expired items are treated as missing.
func (s *Store) Open(name string) (*os.File, Item, error) {
	if s == nil || !NameOK(name) {
		return nil, Item{}, os.ErrNotExist
	}
	s.mu.Lock()
	it := s.items[name]
	var item Item
	if it != nil {
		item = *it
	}
	s.mu.Unlock()
	if it == nil || s.expired(item.Created) {
		return nil, Item{}, os.ErrNotExist
	}
	f, err := os.Open(item.Path)
	if err != nil {
		return nil, Item{}, err
	}
	return f, item, nil
}

// Delete removes name from the index and the disk.
// A missing name is not an error. The file mode is irrelevant.
func (s *Store) Delete(name string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	it := s.items[name]
	delete(s.items, name)
	s.mu.Unlock()
	if it != nil {
		os.Remove(it.Path)
	}
}

// Count is the number of indexed files, including ones past TTL until Sweep.
func (s *Store) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// expired reports whether created is older than TTL. A zero TTL never expires.
func (s *Store) expired(created time.Time) bool {
	if s.TTL <= 0 {
		return false
	}
	return s.now().After(created.Add(s.TTL))
}

// Sweep deletes files older than TTL, then the oldest files until the total
// size is at most MaxBytes. It is safe to call on a timer and from tests.
func (s *Store) Sweep() {
	if s == nil {
		return
	}
	s.mu.Lock()
	var list []*Item
	var total int64
	for name, it := range s.items {
		if s.expired(it.Created) {
			os.Remove(it.Path)
			delete(s.items, name)
			continue
		}
		list = append(list, it)
		total += it.Size
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	capn := s.capBytes()
	for total > capn && len(list) > 0 {
		old := list[0]
		list = list[1:]
		os.Remove(old.Path)
		delete(s.items, old.Name)
		total -= old.Size
	}
	s.mu.Unlock()
}
