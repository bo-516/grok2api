package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/shaoboli/agent-mock/internal/config"
	"github.com/shaoboli/agent-mock/internal/grok"
)

// fakeBin and agentBin are built once in TestMain.
var (
	fakeBin  string
	agentBin string
	fixDir   string
)

// TestMain builds the fixture player and the real agent-mock binary.
func TestMain(m *testing.M) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic(err)
	}
	fixDir = filepath.Join(root, "testdata", "grok")
	dir, err := os.MkdirTemp("", "agent-mock-bins")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakegrok")
	agentBin = filepath.Join(dir, "agent-mock")
	build(root, fakeBin, "./internal/testutil/fakegrok")
	build(root, agentBin, "./cmd/agent-mock")
	os.Exit(m.Run())
}

// build runs go build. A failure panics because TestMain has no *testing.T.
func build(dir, out, pkg string) {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		panic(string(b) + "\n" + err.Error())
	}
}

// lockBuf is a concurrent line buffer for access logs.
type lockBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p under the lock.
func (l *lockBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns the log text.
func (l *lockBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startOpt configures one httptest server backed by fakegrok.
type startOpt struct {
	scenario   string
	login      string
	apiKey     string
	maxConc    int
	queue      time.Duration
	reqTimeout time.Duration
	modelMap   map[string]string
	defModel   string
	keep       bool
	logPrompts bool
	extra      []string
}

// harness is a running server plus its fakegrok files.
type harness struct {
	t   *testing.T
	ts  *httptest.Server
	srv *Server
	log *lockBuf
	dir string
}

// start probes fakegrok and serves on a loopback port.
func start(t *testing.T, o startOpt) *harness {
	t.Helper()
	if o.scenario == "" {
		o.scenario = "text"
	}
	if o.login == "" {
		o.login = "ok"
	}
	if o.maxConc == 0 {
		o.maxConc = 4
	}
	if o.queue == 0 {
		o.queue = 30 * time.Second
	}
	if o.reqTimeout == 0 {
		o.reqTimeout = time.Minute
	}
	dir := t.TempDir()
	env := filteredEnv()
	env = append(env,
		"XAI_API_KEY=secret",
		"FAKEGROK_FIXTURE_DIR="+fixDir,
		"FAKEGROK_SCENARIO="+o.scenario,
		"FAKEGROK_REPORT="+filepath.Join(dir, "report.jsonl"),
		"FAKEGROK_SEQ="+filepath.Join(dir, "seq"),
		"FAKEGROK_PIDFILE="+filepath.Join(dir, "pid"),
		"FAKEGROK_DELETE_LOG="+filepath.Join(dir, "deletes"),
		"FAKEGROK_LOGIN="+o.login,
	)
	env = append(env, o.extra...)
	runner := &grok.Runner{Bin: fakeBin, KeepSessions: o.keep, Environ: func() []string { return append([]string(nil), env...) }}
	p := grok.Probe(t.Context(), fakeBin, runner)
	if o.login == "ok" && !p.ToolsetEmpty {
		t.Fatalf("startup toolset: %s", p.ToolsetError)
	}
	cfg := config.Config{
		Addr: "127.0.0.1:0", APIKey: o.apiKey, GrokBin: fakeBin,
		MaxConcurrency: o.maxConc, QueueTimeout: o.queue, RequestTimeout: o.reqTimeout,
		ModelMap: o.modelMap, DefaultModel: o.defModel, KeepSessions: o.keep, LogPrompts: o.logPrompts,
	}
	if cfg.ModelMap == nil {
		cfg.ModelMap = map[string]string{}
	}
	srv := New(cfg, runner)
	for _, m := range p.Models {
		srv.Known = append(srv.Known, m.ID)
	}
	srv.DefaultModel = p.DefaultModel
	srv.GrokVersion = p.Version
	if p.LoginOK {
		srv.Login = "ok"
	} else {
		srv.Login = "not_logged_in"
	}
	lb := &lockBuf{}
	srv.Log = lb
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, srv: srv, log: lb, dir: dir}
}

// client is an openai-go v3 client pointed at this server.
func (h *harness) client() openai.Client {
	key := h.srv.Config.APIKey
	if key == "" {
		key = "dev"
	}
	return openai.NewClient(option.WithBaseURL(h.ts.URL+"/v1"), option.WithAPIKey(key), option.WithMaxRetries(0))
}

// completions is the addressable Chat Completions service. New has a pointer receiver,
// so the client value has to outlive the call; this helper keeps it on the heap.
func (h *harness) completions() *openai.ChatCompletionService {
	c := h.client()
	return &c.Chat.Completions
}

// report is one fakegrok invocation record.
type report struct {
	Argv   []string `json:"argv"`
	XAI    bool     `json:"xai"`
	Prompt string   `json:"prompt"`
	Mode   int      `json:"mode"`
	Head   string   `json:"head"`
	Body   string   `json:"body"`
}

// userReports returns prompt runs that are not the startup toolset probe.
func (h *harness) userReports() []report {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "report.jsonl"))
	if err != nil {
		h.t.Fatal(err)
	}
	var out []report
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var r report
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			h.t.Fatal(err)
		}
		if strings.Contains(r.Head, "agent-mock-toolset-probe") || strings.Contains(r.Body, "agent-mock-toolset-probe") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// filteredEnv drops XAI_API_KEY and FAKEGROK_* so a test starts from a clean parent.
func filteredEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "XAI_API_KEY" || strings.HasPrefix(key, "FAKEGROK_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// fixtureText reads the assistant text stored in a recorded fixture.
// NDJSON uses the result field. A single JSON object uses text.
func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixDir, name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var text string
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if s, ok := m["text"].(string); ok && m["type"] == nil {
			text = s
		}
		if m["type"] == "result" {
			if s, ok := m["result"].(string); ok {
				text = s
			}
		}
	}
	if text == "" {
		t.Fatalf("no text in %s", name)
	}
	return text
}

// postJSON sends a raw chat body and returns status, headers, and body.
func (h *harness) postJSON(body string, hdr map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.srv.Config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.srv.Config.APIKey)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := readAll(resp)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// readAll reads an HTTP body until EOF.
func readAll(resp *http.Response) ([]byte, error) {
	return io.ReadAll(resp.Body)
}
