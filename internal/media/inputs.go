package media

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxInput is the largest accepted source image. Bigger bodies are invalid_image.
const maxInput = 20 << 20

// fetchTimeout is the limit for one https download, including redirects.
const fetchTimeout = 15 * time.Second

// InputError is a rejected image source. Handlers map it to 400 invalid_image.
// Msg is the client message. It names expired, http, a private address, or the sniff.
type InputError struct {
	// Msg is error.message. It does not include file contents.
	Msg string
}

// Error returns Msg so errors.As and logs see the client text.
func (e *InputError) Error() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Source is one image before it is written under mcwd/in.
// Body is set for a multipart file. URL is set for a data URI, an own media URL,
// or an https URL. Both empty is an error from Stage.
type Source struct {
	// Body is the raw file bytes. Nil when URL is set.
	Body []byte
	// Name is the original filename, used only as a hint. The stored name
	// comes from the sniffed type.
	Name string
	// URL is a data URI or an http(s) URL. Empty for a multipart body.
	URL string
}

// Stager writes input images into mcwd/in/<rid> at 0600 and removes that
// directory when the caller invokes cleanup. Root is the media cwd (mcwd).
type Stager struct {
	// Root is mcwd. Stage creates Root/in/<rid> at 0700.
	Root string
	// Store resolves this server's /v1/media names without a network call.
	// Nil means every media-shaped URL is expired.
	Store *Store
	// AllowHost, when it returns true, skips the private-address check for that
	// host. Tests use it to fetch from a loopback TLS server. Nil denies private IPs.
	AllowHost func(host string) bool
	// Transport replaces the download transport. Nil uses a dialer that rejects
	// loopback, private, and link-local targets. Tests set it to fail on any call.
	Transport http.RoundTripper
}

// NewID returns 32 lowercase hex characters, or an error if the random source fails.
// Stage uses it as the directory name. A short or non-hex id is rejected.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Stage writes each source into Root/in/rid and returns absolute paths.
// cleanup removes the directory. It is safe to call more than once, including
// after a failed Stage (the partial directory is still removed).
// rid must be 32 hex characters so the path cannot escape in/. An empty srcs
// returns an error. The directory mode is 0700 and each file is 0600.
func (s *Stager) Stage(ctx context.Context, rid string, srcs []Source) (paths []string, cleanup func(), err error) {
	cleanup = func() {}
	if s == nil || s.Root == "" {
		return nil, cleanup, &InputError{Msg: "image staging is not configured"}
	}
	if len(rid) != 32 || strings.Trim(rid, "0123456789abcdef") != "" {
		return nil, cleanup, &InputError{Msg: "image staging id is invalid"}
	}
	if len(srcs) == 0 {
		return nil, cleanup, &InputError{Msg: "image is required"}
	}
	dir := filepath.Join(s.Root, "in", rid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	var out []string
	for i, src := range srcs {
		body, err := s.bytes(ctx, src)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		ext, _, ok := sniff(body)
		if !ok || ext == "mp4" {
			cleanup()
			return nil, func() {}, &InputError{Msg: "image must be png, jpeg, or webp"}
		}
		path := filepath.Join(dir, itoa(i+1)+"."+ext)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		out = append(out, path)
	}
	return out, cleanup, nil
}

// bytes resolves one source to sniffed image bytes, enforcing the 20 MiB cap.
// A data URI is decoded here. An own /v1/media URL is read from Store.
// http:// is rejected. https is downloaded with the private-address check.
func (s *Stager) bytes(ctx context.Context, src Source) ([]byte, error) {
	if src.URL == "" {
		if int64(len(src.Body)) > maxInput {
			return nil, &InputError{Msg: "image exceeds 20 MiB"}
		}
		if len(src.Body) == 0 {
			return nil, &InputError{Msg: "image is empty"}
		}
		return src.Body, nil
	}
	if strings.HasPrefix(src.URL, "data:") {
		return decodeData(src.URL)
	}
	if name, ok := OwnMediaName(src.URL); ok {
		return s.fromStore(name)
	}
	u, err := url.Parse(src.URL)
	if err != nil || u.Host == "" {
		return nil, &InputError{Msg: "image URL is invalid"}
	}
	if u.Scheme == "http" {
		return nil, &InputError{Msg: "external http URL is not allowed"}
	}
	if u.Scheme != "https" {
		return nil, &InputError{Msg: "image URL must be https"}
	}
	if err := s.vet(u); err != nil {
		return nil, err
	}
	return s.download(ctx, u)
}

// fromStore copies a live media file. A missing or expired name is invalid_image
// and does not call the network. The message contains "expired".
func (s *Stager) fromStore(name string) ([]byte, error) {
	if s.Store == nil {
		return nil, &InputError{Msg: "media file expired"}
	}
	f, item, err := s.Store.Open(name)
	if err != nil {
		return nil, &InputError{Msg: "media file expired"}
	}
	defer f.Close()
	if item.Size > maxInput {
		return nil, &InputError{Msg: "image exceeds 20 MiB"}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return nil, &InputError{Msg: "media file expired"}
	}
	if int64(len(b)) > maxInput {
		return nil, &InputError{Msg: "image exceeds 20 MiB"}
	}
	return b, nil
}

// OwnMediaName returns the filename when raw is a URL whose path is
// /v1/media/<32 hex>.<ext>. The host is not checked: a name that is not in
// the store is expired rather than fetched. ok is false for any other URL.
func OwnMediaName(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	const prefix = "/v1/media/"
	if !strings.HasPrefix(u.Path, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(u.Path, prefix)
	if strings.Contains(name, "/") || !NameOK(name) {
		return "", false
	}
	return name, true
}

// vet rejects a URL whose host is already a blocked IP, unless AllowHost says
// this test host is permitted. DNS answers are checked again in the dialer.
func (s *Stager) vet(u *url.URL) error {
	if s.AllowHost != nil && s.AllowHost(u.Hostname()) {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && blockedIP(ip) {
		return &InputError{Msg: "URL target is not a public address"}
	}
	return nil
}

// download GETs u over https and returns at most 20 MiB.
// A redirect to a non-https URL fails. The timeout is 15s.
func (s *Stager) download(ctx context.Context, u *url.URL) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &InputError{Msg: "image URL is invalid"}
	}
	res, err := s.client(u.Hostname()).Do(req)
	if err != nil {
		return nil, &InputError{Msg: "image download failed"}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &InputError{Msg: "image download failed"}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxInput+1))
	if err != nil {
		return nil, &InputError{Msg: "image download failed"}
	}
	if int64(len(b)) > maxInput {
		return nil, &InputError{Msg: "image exceeds 20 MiB"}
	}
	return b, nil
}

// client is the downloader. CheckRedirect refuses a hop that leaves https.
// AllowHost skips the private-IP dialer so a test TLS server can answer.
func (s *Stager) client(host string) *http.Client {
	allow := s.AllowHost != nil && s.AllowHost(host)
	tr := s.Transport
	if tr == nil {
		d := &net.Dialer{Timeout: fetchTimeout}
		if !allow {
			d.Control = rejectPrivate
		}
		tr = &http.Transport{DialContext: d.DialContext}
	}
	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("redirect left https")
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !allow {
				if ip := net.ParseIP(req.URL.Hostname()); ip != nil && blockedIP(ip) {
					return errors.New("redirect to a non-public address")
				}
			}
			return nil
		},
	}
}

// rejectPrivate is a Dialer.Control that refuses blocked IPs.
// The address is host:port. A parse failure refuses the dial.
func rejectPrivate(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || blockedIP(ip) {
		return errors.New("refusing non-public address")
	}
	return nil
}

// blockedIP reports loopback, private, link-local, multicast, and unspecified.
// Those targets are invalid_image and are not dialed.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// decodeData accepts data:image/png|jpeg|jpg|webp;base64,... only.
// A bad encoding or a body over 20 MiB is invalid_image. The sniffed type
// is checked later by Stage, so a mislabeled payload still fails.
func decodeData(raw string) ([]byte, error) {
	const prefix = "data:"
	rest := strings.TrimPrefix(raw, prefix)
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, &InputError{Msg: "image data URI must be base64 png, jpeg, or webp"}
	}
	mime := strings.ToLower(strings.TrimSuffix(meta, ";base64"))
	switch mime {
	case "image/png", "image/jpeg", "image/jpg", "image/webp":
	default:
		return nil, &InputError{Msg: "image data URI must be base64 png, jpeg, or webp"}
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(payload)
		if err != nil {
			return nil, &InputError{Msg: "image data URI is not valid base64"}
		}
	}
	if int64(len(b)) > maxInput {
		return nil, &InputError{Msg: "image exceeds 20 MiB"}
	}
	if len(b) == 0 {
		return nil, &InputError{Msg: "image is empty"}
	}
	return b, nil
}

// itoa is a small decimal for file names 1..5. n < 1 becomes 0.
func itoa(n int) string {
	if n < 0 {
		n = 0
	}
	return strconvItoa(n)
}

// strconvItoa avoids importing strconv in a file that is already near the line cap.
func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
