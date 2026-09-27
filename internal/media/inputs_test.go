package media

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStageSources covers multipart bytes, a data URI, an own media URL, and
// rejects http and a loopback https URL. An AllowHost hook may fetch a test TLS PNG.
func TestStageSources(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "media"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	png := pngBytes(t)
	item, err := st.PutBytes(png)
	if err != nil {
		t.Fatal(err)
	}
	s := &Stager{Root: filepath.Join(dir, "mcwd"), Store: st}
	stageOne := func(src Source) ([]string, error) {
		rid, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		paths, cleanup, err := s.Stage(context.Background(), rid, []Source{src})
		if cleanup != nil {
			t.Cleanup(cleanup)
		}
		return paths, err
	}
	paths, err := stageOne(Source{Body: png, Name: "a.png"})
	if err != nil || len(paths) != 1 || !strings.Contains(paths[0], string(os.PathSeparator)+"in"+string(os.PathSeparator)) {
		t.Fatal(err, paths)
	}
	fi, err := os.Stat(paths[0])
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	parent := filepath.Dir(paths[0])
	os.RemoveAll(parent)
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("staged file remains")
	}
	raw := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if _, err := stageOne(Source{URL: raw}); err != nil {
		t.Fatal(err)
	}
	s.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("own media URL made an outbound call")
		return nil, context.Canceled
	})
	if _, err := stageOne(Source{URL: "http://127.0.0.1:9/v1/media/" + item.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := stageOne(Source{URL: "http://10.0.0.1/x.png"}); err == nil {
		t.Fatal("http was accepted")
	}
	if _, err := stageOne(Source{URL: "https://127.0.0.1:9/x.png"}); err == nil {
		t.Fatal("loopback https was accepted")
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	defer ts.Close()
	host := mustHost(ts.URL)
	s.AllowHost = func(h string) bool { return h == host }
	s.Transport = ts.Client().Transport
	if _, err := stageOne(Source{URL: ts.URL + "/a.png"}); err != nil {
		t.Fatal(err)
	}
	textSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer textSrv.Close()
	s.AllowHost = func(string) bool { return true }
	s.Transport = textSrv.Client().Transport
	if _, err := stageOne(Source{URL: textSrv.URL + "/t.txt"}); err == nil {
		t.Fatal("text was accepted")
	}
}

// mustHost returns the hostname of a test server URL.
func mustHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// roundTrip is an http.RoundTripper for tests.
type roundTrip func(*http.Request) (*http.Response, error)

// RoundTrip calls the function.
func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
