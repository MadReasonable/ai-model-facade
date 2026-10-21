// Command ai-model-facade is an OpenAI-compatible gateway serving virtual models.
//
//	ai-model-facade serve <config>    run the gateway
//	ai-model-facade check <config>    validate the config and print each virtual
//	                                  model's resolved params per candidate
//	ai-model-facade health <config>   exit 0 if the gateway at server.listen
//	                                  answers /health (for container health checks)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/gateway"
)

func main() {
	commands := map[string]func(string) error{"serve": serve, "check": check, "health": health}
	command := commands[""]
	if len(os.Args) == 3 {
		command = commands[os.Args[1]]
	}
	if command == nil {
		fmt.Fprintln(os.Stderr, "usage: ai-model-facade serve|check|health <config>")
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	g, err := gateway.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              g.Config.Server.Listen,
		Handler:           g.Handler(log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errs := make(chan error, 1)
	go func() {
		errs <- srv.ListenAndServe()
	}()
	log.Info("listening", "addr", srv.Addr, "models", len(g.Models))
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
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
	return 0, fmt.Errorf("logging.level: unknown level %q (debug | info | warning | error)", s)
}
