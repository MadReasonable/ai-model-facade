// Command ai-model-facade is an OpenAI-compatible gateway serving virtual models.
//
//	ai-model-facade serve <config>    run the gateway
//	ai-model-facade check <config>    validate the config and print each virtual
//	                                  model's resolved params per candidate
//	ai-model-facade health <config>   exit 0 if the gateway at server.listen
//	                                  answers /health (for container health checks)
//	ai-model-facade version           print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/gateway"
)

// about precedes the usage line, for someone who runs the binary without
// knowing what it is. It is the README's summary.
const about = `An OpenAI-compatible gateway that gives clients a stable model name
and decides, at request time, which real model answers.
https://github.com/MadReasonable/ai-model-facade
`

// usage matches the README's Commands section.
const usage = `usage:
  ai-model-facade serve <config>    run the gateway
  ai-model-facade check <config>    validate the config and print each candidate's resolved params
  ai-model-facade health <config>   exit 0 if the gateway at server.listen answers /health, 1 if not
  ai-model-facade version           print the version, which the startup log line also reports
`

// version is set at build time by the release workflow and the Dockerfile:
// -ldflags "-X main.version=v0.1.0".
var version = ""

// currentVersion is the version set at build time, else the module version
// that `go install ...@v0.1.0` or a build in a git checkout records, else
// "dev".
func currentVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run carries out the command in args and returns the exit code: 0 on
// success, 1 if the command failed, 2 if args name no command.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, currentVersion())
		return 0
	}
	commands := map[string]func(path string, stdout, stderr io.Writer) error{"serve": serve, "check": check, "health": health}
	command := commands[""]
	if len(args) == 2 {
		command = commands[args[0]]
	}
	if command == nil {
		fmt.Fprint(stderr, about+"\n"+usage+"\n")
		return 2
	}
	if err := command(args[1], stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func check(path string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	// Startup notes are part of the printed description, so don't log them too.
	g, err := gateway.Build(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	for i, m := range g.Models {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout, m.Name)
		m.Handler.Describe(stdout)
	}
	// serve starts despite these; check is where they should be caught.
	for _, w := range g.Warnings {
		fmt.Fprintln(stderr, "error:", w)
	}
	if n := len(g.Warnings); n > 0 {
		return fmt.Errorf("%d check(s) against the backends failed", n)
	}
	return nil
}

// health checks a running gateway. It only parses the config to find
// server.listen; it contacts no backend.
func health(path string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	url, err := gateway.HealthURL(cfg.Server.Listen)
	if err != nil {
		return err
	}
	return gateway.CheckHealth(context.Background(), url)
}

func serve(path string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	level, err := parseLevel(cfg.Logging.Level)
	if err != nil {
		return err
	}
	var levelVar slog.LevelVar // reloadable
	levelVar.Set(level)
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: &levelVar}))
	g, err := gateway.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	live := gateway.NewLive(g, log)
	srv := &http.Server{
		Addr:              g.Config.Server.Listen,
		Handler:           live,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Listen before logging "listening", so the log line, and anything that
	// waits for it, never comes before the port is open.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	errs := make(chan error, 1)
	go func() {
		errs <- srv.Serve(ln)
	}()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	log.Info("listening", "addr", srv.Addr, "models", len(g.Models), "version", currentVersion())
	for running := true; running; {
		select {
		case err := <-errs:
			return err
		case <-hup:
			if err := reload(path, live, &levelVar, log); err != nil {
				log.Error("config reload failed; keeping the running config", "error", err)
			}
		case <-ctx.Done():
			running = false
		}
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// reload re-reads the config on SIGHUP. Nothing changes unless the whole
// config loads and validates; checks against the backends only warn, as at
// startup.
func reload(path string, live *gateway.Live, levelVar *slog.LevelVar, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	level, err := parseLevel(cfg.Logging.Level)
	if err != nil {
		return err
	}
	g, err := live.Reload(ctx, cfg)
	if err != nil {
		return err
	}
	levelVar.Set(level)
	log.Info("config reloaded", "models", len(g.Models))
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("logging.level: unknown level '%s' (debug | info | warning | error)", s)
}
