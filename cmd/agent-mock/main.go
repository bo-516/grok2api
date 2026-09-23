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
	"strings"
	"syscall"
	"time"

	"github.com/shaoboli/agent-mock/internal/config"
	"github.com/shaoboli/agent-mock/internal/grok"
	"github.com/shaoboli/agent-mock/internal/server"
	"github.com/shaoboli/agent-mock/internal/version"
)

// main parses config, probes grok, listens, and on SIGINT kills in-flight grok groups.
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
	runner := &grok.Runner{Bin: cfg.GrokBin, KeepSessions: cfg.KeepSessions}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 14*time.Second)
	probed := grok.Probe(probeCtx, cfg.GrokBin, runner)
	cancelProbe()
	srv := server.New(cfg, runner)
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
		os.Exit(1)
	}
	// Print only after the port accepts connections so the listen line is already true.
	fmt.Fprint(os.Stderr, grok.FormatStartup(version.Version, listenBase(cfg.Addr), cfg.APIKey, cfg.MaxConcurrency, probed))
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-sigCtx.Done():
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	runner.Close()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
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

// modelIDs copies probe models into the server's known-id list.
func modelIDs(models []grok.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}
