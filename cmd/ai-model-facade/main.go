// Command ai-model-facade is an LLM gateway serving virtual models to OpenAI and Claude clients.
//
//	ai-model-facade serve <config> [--secrets <file>]
//	                                  run the gateway
//	ai-model-facade check <config> [--secrets <file>]
//	                                  validate the config and print each virtual
//	                                  model's resolved params per candidate
//	ai-model-facade health <config>   exit 0 if the gateway at server.listen
//	                                  answers /health (for container health checks)
//	ai-model-facade hash-key          print a new key and its hash for server.apiKeys
//	ai-model-facade hash-key --stdin  print the hash of a key read from stdin
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
	"strings"
	"syscall"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
	"github.com/MadReasonable/ai-model-facade/internal/gateway"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
)

// about precedes the usage line, for someone who runs the binary without
// knowing what it is. It is the README's summary.
const about = `An LLM gateway that gives OpenAI and Claude clients a stable model
name and decides, at request time, which real model answers.
https://github.com/MadReasonable/ai-model-facade
`

// usage matches the README's Commands section.
const usage = `usage:
  ai-model-facade serve <config>    run the gateway
  ai-model-facade check <config>    validate the config and print each candidate's resolved params
                                    serve and check take --secrets <file> after the config,
                                    a secrets file merged into it
  ai-model-facade health <config>   exit 0 if the gateway at server.listen answers /health, 1 if not
  ai-model-facade hash-key          print a new key and its hash for server.apiKeys
  ai-model-facade hash-key --stdin  print the hash of a key read from standard input
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
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run carries out the command in args and returns the exit code: 0 on
// success, 1 if the command failed, 2 if args name no command.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, currentVersion())
		return 0
	}
	if len(args) >= 1 && args[0] == "hash-key" && len(args) <= 2 {
		fromStdin := len(args) == 2 && args[1] == "--stdin"
		if len(args) == 2 && !fromStdin {
			fmt.Fprint(stderr, about+"\n"+usage+"\n")
			return 2
		}
		if err := hashKey(fromStdin, stdin, stdout); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	commands := map[string]func(files configFiles, stdout, stderr io.Writer) error{"serve": serve, "check": check, "health": health}
	command := commands[""]
	var files configFiles
	switch {
	case len(args) == 2:
		command, files = commands[args[0]], configFiles{config: args[1]}
	case len(args) == 4 && args[2] == "--secrets" && args[0] != "health":
		command, files = commands[args[0]], configFiles{config: args[1], secrets: args[3]}
	}
	if command == nil {
		fmt.Fprint(stderr, about+"\n"+usage+"\n")
		return 2
	}
	if err := command(files, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// configFiles names the files a command loads: the config, and the secrets
// file merged into it, if --secrets names one.
type configFiles struct {
	config, secrets string
}

func (f configFiles) load() (*config.Config, error) {
	return config.Load(f.config, f.secrets)
}

func check(files configFiles, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := files.load()
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
	// Notes have a defined meaning, so they don't fail check either.
	for _, note := range cfg.Notes {
		fmt.Fprintln(stderr, "warning:", note)
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

// hashKey prints a new random key and the hash line for server.apiKeys, or,
// fromStdin, the hash line of a key read from stdin. A key to hash is read
// from stdin rather than taken as an argument, which would leave it in the
// shell's history and in the process list.
func hashKey(fromStdin bool, stdin io.Reader, stdout io.Writer) error {
	if !fromStdin {
		key := config.NewKey()
		fmt.Fprintf(stdout, "key:  %s\nhash: %s\n", key, config.HashKey(key))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return err
	}
	key := strings.TrimRight(string(data), "\r\n")
	if key == "" {
		return errors.New("no key on standard input")
	}
	fmt.Fprintf(stdout, "hash: %s\n", config.HashKey(key))
	return nil
}

// health checks a running gateway. It only parses the config to find
// server.listen; it contacts no backend.
// health reads only server.listen, since the image's health check passes
// no secrets file, and a gateway that is up is healthy whatever else its
// config holds.
func health(files configFiles, stdout, stderr io.Writer) error {
	listen, err := config.Listen(files.config)
	if err != nil {
		return err
	}
	url, err := gateway.HealthURL(listen)
	if err != nil {
		return err
	}
	return gateway.CheckHealth(context.Background(), url)
}

// newLogger returns serve's logger: logs, the copy of the log for GET
// /amf/logs, then stderr at level. The copy comes first, so a record is in
// memory before a stderr write that stalls can hold it up.
func newLogger(stderr io.Writer, level *slog.LevelVar, logs *loghub.Hub) *slog.Logger {
	return slog.New(slog.NewMultiHandler(logs.Handler(), slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})))
}

func serve(files configFiles, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := files.load()
	if err != nil {
		return err
	}
	level, err := cfg.Logging.SlogLevel()
	if err != nil {
		return err
	}
	var levelVar slog.LevelVar // reloadable
	levelVar.Set(level)
	logs := loghub.New()
	logs.Configure(cfg)
	log := newLogger(stderr, &levelVar, logs)
	g, err := gateway.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	live := gateway.NewLive(g, log, &levelVar, configfile.Files{Config: files.config, Secrets: files.secrets})
	live.KeepLogs(logs)
	srv := &http.Server{
		Addr:              g.Config.Server.Listen,
		Handler:           live,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	srv.RegisterOnShutdown(logs.EndStreams) // a log stream never ends by itself
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
			live.ReloadFiles("SIGHUP") // logs its outcome
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
