// Command linkly runs the linkly URL shortener.
//
// Configuration comes from LINKLY_* environment variables; see the README.
// `linkly -healthcheck` probes a running instance and exits 0 when it is
// healthy, which lets distroless containers define a HEALTHCHECK without a
// shell or curl.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/app"
	"github.com/SAMARTHJAISWAL/linkly/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "linkly:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("linkly", flag.ContinueOnError)
	healthcheck := fs.Bool("healthcheck", false, "probe /healthz on LINKLY_ADDR and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, "linkly", version)
		return nil
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if *healthcheck {
		return probe(cfg.Addr)
	}

	log := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		a.Close()
		return err
	}
	log.Info("starting linkly", "version", version, "db", cfg.DBPath,
		"rate_limit", cfg.RateLimit, "rate_burst", cfg.RateBurst,
		"trust_proxy", cfg.TrustProxy, "delete_enabled", cfg.AdminToken != "")
	return a.Run(ctx, ln)
}

// probe GETs /healthz on the local listen address.
func probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}
