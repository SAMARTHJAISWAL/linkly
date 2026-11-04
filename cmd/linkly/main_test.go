package main

import (
	"bytes"
	"strings"
	"testing"
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
