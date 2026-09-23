// Package config loads agent-mock settings from flags and AGENT_MOCK_* environment variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ErrHelp is returned when the caller asked for -h or -help.
// main prints Help and exits 0. Treating help as a config error would exit 2.
var ErrHelp = errors.New("help")

// Error is a configuration failure that must stop the process.
// Exit is the process status (2 for usage and safety refusals). Msg is stderr text.
type Error struct {
	// Exit is the process status code. Callers must os.Exit(Exit) after printing Msg.
	Exit int
	// Msg is the complete stderr line, without a trailing newline.
	Msg string
}

// Error returns Msg so the failure can be logged or wrapped.
func (e *Error) Error() string { return e.Msg }

// Config is the validated process configuration.
// A non-loopback Addr with an empty APIKey is rejected so the subscription cannot
// be exposed on a network interface without a bearer token.
type Config struct {
	// Addr is host:port to listen on. Default 127.0.0.1:8787.
	// Non-loopback values require APIKey; otherwise startup returns exit 2.
	Addr string
	// APIKey, when non-empty, is the required Authorization Bearer token.
	// Empty on loopback means the server does not check a key.
	APIKey string
	// GrokBin is the grok executable path or PATH name. Default "grok".
	// A missing file surfaces later as HTTP 500 grok_not_found, not at flag parse.
	GrokBin string
	// DefaultModel is passed as -m when the request model is unknown to grok and
	// is not a ModelMap alias. Empty means omit -m and let grok pick its default.
	DefaultModel string
	// ModelMap maps caller model ids (gpt-4o) to grok model ids.
	// Flag wins over AGENT_MOCK_MODEL_MAP. Unknown aliases fall through to DefaultModel.
	ModelMap map[string]string
	// ReasoningEffort is the default --reasoning-effort when the request omits one.
	// Empty leaves grok's own default. An illegal request value is rejected later with 400.
	ReasoningEffort string
	// MaxConcurrency is how many grok processes may run at once. Default 4.
	// Extra requests wait up to QueueTimeout.
	MaxConcurrency int
	// QueueTimeout is how long a request waits for a free grok slot.
	// Past this, the server returns 429 agent_mock_busy.
	QueueTimeout time.Duration
	// RequestTimeout is the hard limit for one grok run.
	// Expiry SIGTERMs the process group and returns 504 grok_timeout.
	RequestTimeout time.Duration
	// KeepSessions leaves grok sessions on disk. Default false deletes them
	// with a non-interactive `grok sessions delete`.
	KeepSessions bool
	// LogPrompts includes the rendered prompt on the single access-log line.
	// Default false, so prompt text never reaches the log.
	LogPrompts bool
	// ShowVersion means -version was set. main prints the version and exits 0
	// without listening or probing grok.
	ShowVersion bool
}

// Help is the -help text. Flag names match the design doc so operators can paste them.
const Help = `agent-mock: OpenAI-compatible LLM endpoint for local dev, backed by your Grok Build login.
  -addr string               listen address (default "127.0.0.1:8787")
  -api-key string            require "Authorization: Bearer <key>"; mandatory when -addr is not loopback
  -grok-bin string           grok executable (default "grok" from PATH)
  -default-model string      grok model for request models grok does not know (default: grok's own default)
  -model-map value           alias=grokModel, repeatable, e.g. gpt-4o=grok-4.7
  -reasoning-effort string   default --reasoning-effort passed to grok (default: grok's own default)
  -max-concurrency int       simultaneous grok runs (default 4)
  -queue-timeout duration    max wait for a free run slot before 429 (default 30s)
  -request-timeout duration  hard limit per grok run (default 3m0s)
  -keep-sessions             keep the grok sessions agent-mock creates (default: delete them)
  -log-prompts               log rendered prompts (default off)
  -version                   print version and exit
`

// Parse reads args (not including argv0) and fills any unset flag from getenv.
// getenv is called as getenv("AGENT_MOCK_ADDR"). When a flag and its env var are
// both set, the flag wins. -version skips validation. -h returns ErrHelp.
// A non-loopback Addr without APIKey returns *Error{Exit:2}. Other usage errors
// also return *Error{Exit:2}. A nil error means the config is safe to run.
func Parse(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	fs := flag.NewFlagSet("agent-mock", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	cfg := Config{ModelMap: map[string]string{}}
	maps := modelMap{dst: cfg.ModelMap}
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1:8787", "listen address")
	fs.StringVar(&cfg.APIKey, "api-key", "", "bearer token; required when -addr is not loopback")
	fs.StringVar(&cfg.GrokBin, "grok-bin", "grok", "grok executable")
	fs.StringVar(&cfg.DefaultModel, "default-model", "", "model used when grok does not know the request model")
	fs.Var(&maps, "model-map", "alias=grokModel, repeatable")
	fs.StringVar(&cfg.ReasoningEffort, "reasoning-effort", "", "default --reasoning-effort")
	fs.IntVar(&cfg.MaxConcurrency, "max-concurrency", 4, "simultaneous grok runs")
	fs.DurationVar(&cfg.QueueTimeout, "queue-timeout", 30*time.Second, "wait for a free grok slot")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", 3*time.Minute, "hard limit per grok run")
	fs.BoolVar(&cfg.KeepSessions, "keep-sessions", false, "keep grok sessions")
	fs.BoolVar(&cfg.LogPrompts, "log-prompts", false, "log rendered prompts")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, ErrHelp
		}
		return Config{}, &Error{Exit: 2, Msg: err.Error()}
	}
	if cfg.ShowVersion {
		return cfg, nil
	}
	if err := applyEnv(fs, getenv); err != nil {
		return Config{}, &Error{Exit: 2, Msg: err.Error()}
	}
	cfg.ModelMap = maps.dst
	if err := validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv sets each flag that the user did not pass from AGENT_MOCK_<UPPER_SNAKE>.
// A flag that was present on the command line is left alone, even when the env var is set.
// A value the flag cannot parse (a bad duration, a bad model-map) is returned and startup exits 2.
func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	var setErr error
	fs.VisitAll(func(f *flag.Flag) {
		if setErr != nil || seen[f.Name] {
			return
		}
		key := "AGENT_MOCK_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		val := getenv(key)
		if val == "" {
			return
		}
		if err := f.Value.Set(val); err != nil {
			setErr = fmt.Errorf("%s: %w", key, err)
		}
	})
	return setErr
}

// validate rejects settings that would expose the subscription or hang forever.
// It returns *Error with exit 2. Addr must be host:port. Durations must be positive.
func validate(cfg *Config) error {
	if cfg.Addr == "" {
		return &Error{Exit: 2, Msg: "-addr is required"}
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return &Error{Exit: 2, Msg: fmt.Sprintf("-addr %q must be host:port", cfg.Addr)}
	}
	if !IsLoopback(cfg.Addr) && cfg.APIKey == "" {
		return &Error{Exit: 2, Msg: "-api-key is required when -addr is not loopback"}
	}
	if cfg.MaxConcurrency < 1 {
		return &Error{Exit: 2, Msg: "-max-concurrency must be >= 1"}
	}
	if cfg.QueueTimeout <= 0 {
		return &Error{Exit: 2, Msg: "-queue-timeout must be > 0"}
	}
	if cfg.RequestTimeout <= 0 {
		return &Error{Exit: 2, Msg: "-request-timeout must be > 0"}
	}
	if cfg.GrokBin == "" {
		return &Error{Exit: 2, Msg: "-grok-bin is required"}
	}
	return nil
}

// IsLoopback reports whether addr (host:port or a bare host) is a loopback address.
// localhost, 127.0.0.0/8, and ::1 are loopback. 0.0.0.0 and public hosts are not.
// A parse failure returns false so the caller demands an API key instead of exposing grok.
func IsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// modelMap is a repeatable flag.Value for alias=grokModel pairs.
// Commas separate pairs so AGENT_MOCK_MODEL_MAP can carry more than one.
// A missing '=' or an empty side returns an error and the process exits 2.
type modelMap struct {
	// dst receives aliases. It is the map stored on Config.
	dst map[string]string
}

// String renders the map for flag help. Order is not significant.
func (m *modelMap) String() string {
	if m == nil || len(m.dst) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.dst))
	for k, v := range m.dst {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

// Set adds one or more alias=grokModel pairs. An invalid pair returns an error
// and flag parsing fails, so the process does not start with a broken route.
func (m *modelMap) Set(v string) error {
	if m.dst == nil {
		m.dst = map[string]string{}
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		alias, grokModel, ok := strings.Cut(part, "=")
		alias = strings.TrimSpace(alias)
		grokModel = strings.TrimSpace(grokModel)
		if !ok || alias == "" || grokModel == "" {
			return fmt.Errorf("invalid -model-map %q (want alias=grokModel)", part)
		}
		m.dst[alias] = grokModel
	}
	return nil
}
