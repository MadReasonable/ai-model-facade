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
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(currentVersion())
		return
	}
	commands := map[string]func(string) error{"serve": serve, "check": check, "health": health}
	command := commands[""]
	if len(os.Args) == 3 {
		command = commands[os.Args[1]]
	}
	if command == nil {
		fmt.Fprint(os.Stderr, about+"\n"+usage+"\n")
		os.Exit(2)
	}
	err := command(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func check(path string) error {
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
			fmt.Println()
		}
		fmt.Println(m.Name)
		m.Handler.Describe(os.Stdout)
	}
	// serve starts despite these; check is where they should be caught.
	for _, w := range g.Warnings {
		fmt.Fprintln(os.Stderr, "error:", w)
	}
	if n := len(g.Warnings); n > 0 {
		return fmt.Errorf("%d check(s) against the backends failed", n)
	}
	return nil
}

// health checks a running gateway. It only parses the config to find
// server.listen; it contacts no backend.
func health(path string) error {
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

func serve(path string) error {
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &levelVar}))
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
