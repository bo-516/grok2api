package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestParseDefaults checks the documented defaults when no flags or env are set.
func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8787" || cfg.GrokBin != "grok" || cfg.MaxConcurrency != 4 {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.QueueTimeout != 30*time.Second || cfg.RequestTimeout != 3*time.Minute {
		t.Fatalf("durations: %s %s", cfg.QueueTimeout, cfg.RequestTimeout)
	}
	if cfg.KeepSessions || cfg.LogPrompts || cfg.APIKey != "" {
		t.Fatalf("bool defaults: %+v", cfg)
	}
}

// TestFlagWinsOverEnv checks that a command-line flag beats AGENT_MOCK_*.
func TestFlagWinsOverEnv(t *testing.T) {
	cfg, err := Parse([]string{"-addr", "127.0.0.1:9", "-max-concurrency", "2"}, func(k string) string {
		if k == "AGENT_MOCK_ADDR" {
			return "127.0.0.1:1"
		}
		if k == "AGENT_MOCK_MAX_CONCURRENCY" {
			return "9"
		}
		if k == "AGENT_MOCK_API_KEY" {
			return "from-env"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9" || cfg.MaxConcurrency != 2 || cfg.APIKey != "from-env" {
		t.Fatalf("cfg %+v", cfg)
	}
}

// TestNonLoopbackRequiresKey is the exit-2 guard for a public listen address.
func TestNonLoopbackRequiresKey(t *testing.T) {
	_, err := Parse([]string{"-addr", "0.0.0.0:8787"}, func(string) string { return "" })
	var ce *Error
	if !errors.As(err, &ce) || ce.Exit != 2 || !strings.Contains(ce.Msg, "-api-key is required when -addr is not loopback") {
		t.Fatalf("err=%v", err)
	}
	cfg, err := Parse([]string{"-addr", "0.0.0.0:8787", "-api-key", "s3cret"}, func(string) string { return "" })
	if err != nil || cfg.APIKey != "s3cret" {
		t.Fatalf("with key: %+v %v", cfg, err)
	}
}

// TestModelMap parses repeatable alias pairs and the comma form used by the env var.
func TestModelMap(t *testing.T) {
	cfg, err := Parse([]string{"-model-map", "gpt-4o=grok-4.7", "-model-map", "gpt-4o-mini=grok-4.6"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelMap["gpt-4o"] != "grok-4.7" || cfg.ModelMap["gpt-4o-mini"] != "grok-4.6" {
		t.Fatalf("%v", cfg.ModelMap)
	}
	cfg, err = Parse(nil, func(k string) string {
		if k == "AGENT_MOCK_MODEL_MAP" {
			return "a=b,c=d"
		}
		return ""
	})
	if err != nil || cfg.ModelMap["a"] != "b" || cfg.ModelMap["c"] != "d" {
		t.Fatalf("%v %v", cfg.ModelMap, err)
	}
}

// TestHelpAndVersion do not require a usable address.
func TestHelpAndVersion(t *testing.T) {
	_, err := Parse([]string{"-h"}, func(string) string { return "" })
	if !errors.Is(err, ErrHelp) {
		t.Fatal(err)
	}
	cfg, err := Parse([]string{"-version"}, func(string) string { return "" })
	if err != nil || !cfg.ShowVersion {
		t.Fatal(err)
	}
	if !strings.Contains(Help, "-keep-sessions") || !strings.Contains(Help, "-log-prompts") || !strings.Contains(Help, "-reasoning-effort") {
		t.Fatal("help missing flags")
	}
}

// TestLoopback accepts 127.0.0.1 and ::1 and rejects 0.0.0.0.
func TestLoopback(t *testing.T) {
	if !IsLoopback("127.0.0.1:8787") || !IsLoopback("[::1]:8787") || !IsLoopback("localhost:1") {
		t.Fatal("expected loopback")
	}
	if IsLoopback("0.0.0.0:8787") || IsLoopback("192.168.1.1:1") {
		t.Fatal("expected non-loopback")
	}
}
