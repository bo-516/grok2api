// Command agent-mock serves an OpenAI-compatible chat endpoint backed by the local grok CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shaoboli/agent-mock/internal/config"
	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/media"
	"github.com/shaoboli/agent-mock/internal/server"
	"github.com/shaoboli/agent-mock/internal/version"
)

// exitGrace bounds how long a failed start waits for background session deletes
// (at least the startup probe's) before the process exits.
const exitGrace = 5 * time.Second

// main parses config, sweeps leftovers of earlier runs, probes grok, listens, and on
// SIGINT kills in-flight grok groups, then lets pending session deletes finish.
// The whole shutdown shares one 10 s budget. A delete that does not finish in time
// stays recorded under the temp root and the next start retries it.
// A non-loopback address without -api-key exits 2 before the socket opens.
// -version prints the version and does not probe grok. -help exits 0.
func main() {
	cfg, err := config.Parse(os.Args[1:], os.Getenv)
	if errors.Is(err, config.ErrHelp) {
		fmt.Fprint(os.Stdout, config.Help)
		os.Exit(0)
	}
	var ce *config.Error
	if errors.As(err, &ce) {
		fmt.Fprintln(os.Stderr, ce.Msg)
		os.Exit(ce.Exit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
	if cfg.ShowVersion {
		fmt.Printf("agent-mock v%s\n", version.Version)
		os.Exit(0)
	}
	runner := &grok.Runner{Bin: cfg.GrokBin, KeepSessions: cfg.KeepSessions, Log: os.Stderr}
	// A killed or timed-out earlier process can leave sessions and prompt history.
	runner.Sweep()
	var store *media.Store
	var jobs *media.Jobs
	if cfg.Media {
		_ = os.RemoveAll(filepath.Join(runner.RootDir(), "mcwd", "in"))
		var err error
		store, err = media.NewStore(filepath.Join(runner.RootDir(), "media"), cfg.MediaTTL)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		jobs = media.NewJobs(cfg.MaxVideoJobs, cfg.MediaTTL)
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 14*time.Second)
	var probed grok.ProbeResult
	var mediaProbe grok.MediaProbe
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go func() {
		defer probeWG.Done()
		probed = grok.Probe(probeCtx, cfg.GrokBin, runner)
	}()
	if cfg.Media {
		probeWG.Add(1)
		go func() {
			defer probeWG.Done()
			mediaProbe = grok.ProbeMedia(probeCtx, runner)
		}()
	}
	probeWG.Wait()
	cancelProbe()
	probed.MediaChecked = true
	probed.MediaKeep = shortKeep(cfg.MediaTTL)
	if cfg.Media {
		probed.MediaOffered = mediaProbe.Offered
		probed.MediaMissing = mediaProbe.Missing
	} else {
		probed.MediaOff = true
	}
	srv := server.New(cfg, runner)
	if cfg.Media {
		srv.Store = store
		srv.Stager = &media.Stager{Root: filepath.Join(runner.RootDir(), "mcwd"), Store: store}
		srv.Jobs = jobs
		if mediaProbe.Offered == nil {
			srv.MediaTools = []string{}
		} else {
			srv.MediaTools = append([]string(nil), mediaProbe.Offered...)
		}
	}
	srv.Known = modelIDs(probed.Models)
	srv.DefaultModel = probed.DefaultModel
	srv.GrokVersion = probed.Version
	srv.Log = os.Stderr
	if probed.LoginOK {
		srv.Login = "ok"
	} else {
		srv.Login = "not_logged_in"
	}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if err := waitListening(cfg.Addr, errCh); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		exitAfterDeletes(runner, 1)
	}
	// Print only after the port accepts connections so the listen line is already true.
	fmt.Fprint(os.Stderr, grok.FormatStartup(version.Version, listenBase(cfg.Addr), cfg.APIKey, cfg.MaxConcurrency, probed))
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.Media && store != nil {
		go func() {
			tick := time.NewTicker(5 * time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-sigCtx.Done():
					return
				case <-tick.C:
					store.Sweep()
					if jobs != nil {
						jobs.Sweep()
					}
				}
			}
		}()
	}
	select {
	case <-sigCtx.Done():
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, err.Error())
		exitAfterDeletes(runner, 1)
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runner.Close()
	_ = httpSrv.Shutdown(shutCtx)
	// Handlers have returned, so every run has started its session delete.
	runner.Wait(shutCtx)
}

// exitAfterDeletes waits up to exitGrace for background session deletes, then exits
// with code. Without the wait, the probe session of a start that failed to listen
// would stay on disk until the next start.
func exitAfterDeletes(runner *grok.Runner, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), exitGrace)
	runner.Wait(ctx)
	cancel()
	os.Exit(code)
}

// waitListening returns when addr accepts a TCP connection or the listener reports an error.
// A bind failure is returned so the process exits before claiming the port is open.
func waitListening(addr string, errCh <-chan error) error {
	dead := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case lerr := <-errCh:
			return lerr
		default:
		}
		if time.Now().After(dead) {
			return fmt.Errorf("listen %s failed: %w", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// listenBase turns host:port into the OpenAI base URL, including /v1.
// IPv6 hosts are bracketed. A bad address is returned with http:// so the banner still prints.
func listenBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/v1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + port + "/v1"
}

// shortKeep prints a TTL the way the banner does. Whole hours stay as 1h.
// Other values use Go's duration string. A zero duration is 0s.
func shortKeep(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}

// modelIDs copies probe models into the server's known-id list.
func modelIDs(models []grok.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}
