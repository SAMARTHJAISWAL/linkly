package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/config"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"-version"}, func(string) string { return "" }, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "linkly ") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	env := map[string]string{"LINKLY_RATE_BURST": "-1"}
	err := run(nil, func(k string) string { return env[k] }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LINKLY_RATE_BURST") {
		t.Fatalf("err = %v", err)
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	env := map[string]string{"LINKLY_ADDR": "127.0.0.1:1"}
	if err := run([]string{"-healthcheck"}, func(k string) string { return env[k] }, &bytes.Buffer{}); err == nil {
		t.Fatal("healthcheck succeeded with no server")
	}
}

// TestDockerfileStopsGracefully: the image must send the signal main
// handles, and the default shutdown budget must end before docker stop's
// default 10s grace period does.
func TestDockerfileStopsGracefully(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^STOPSIGNAL SIGTERM$`).Match(b) {
		t.Error("Dockerfile does not declare STOPSIGNAL SIGTERM")
	}
	if d := config.Default().ShutdownTimeout; d >= 10*time.Second {
		t.Errorf("default shutdown timeout %v leaves no margin before docker's 10s SIGKILL", d)
	}
}
