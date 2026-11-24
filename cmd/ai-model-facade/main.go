// Command ai-model-facade is an LLM gateway serving virtual models to OpenAI and Claude clients.
//
//	ai-model-facade init <config> [--secrets <file>]
//	                                  write a starting config with a new admin
//	                                  key, to finish in the admin UI
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
	"io/fs"
	"log/slog"
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
  ai-model-facade init <config>     write a starting config with a new admin key, to finish in the admin UI
  ai-model-facade serve <config>    run the gateway
  ai-model-facade check <config>    validate the config and print each candidate's resolved params
                                    init, serve and check take --secrets <file> after the config,
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
	commands := map[string]func(files configFiles, stdout, stderr io.Writer) error{"init": initFiles, "serve": serve, "check": check, "health": health}
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

// initFiles writes a starting config at files.config, and a secrets file
// at files.secrets if one is named, with a new key for an admin, which it
// prints once: enough to start the gateway and set the rest up in the admin
// UI. The files hold the key's hash, the secrets file if there is one, so
// the config holds no credential. It replaces no file, so it can't destroy
// a config, and builds a gateway from what it wrote as serve would, so what
// it leaves starts.
func initFiles(files configFiles, stdout, _ io.Writer) error {
	paths := []string{files.config}
	if files.secrets != "" {
		paths = append(paths, files.secrets)
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s already exists; init writes a new config and never replaces one", p)
		}
	}
	key := config.NewKey()
	entry := "    - {name: admin, hash: " + config.HashKey(key) + "}\n"
	main := initConfig
	if files.secrets == "" {
		main = strings.Replace(main, initKeys, "  apiKeys:\n"+entry, 1)
	} else {
		main = strings.Replace(main, initKeys, "  # The admin key, by its hash, is in the secrets file.\n", 1)
	}
	var written []string
	write := func(path, content string, mode os.FileMode) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; init writes a new config and never replaces one", path)
		}
		if err != nil {
			return err
		}
		written = append(written, path)
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	err := write(files.config, main, 0o644)
	if err == nil && files.secrets != "" {
		err = write(files.secrets, initSecrets+"server:\n  apiKeys:\n"+entry, 0o600)
	}
	if err == nil {
		// Built as serve builds it, which checks more than loading does.
		var cfg *config.Config
		if cfg, err = files.load(); err == nil {
			_, err = gateway.Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}
	}
	if err != nil {
		for _, p := range written {
			os.Remove(p) // init leaves both files or neither
		}
		return err
	}
	serveArgs := files.config
	if files.secrets != "" {
		serveArgs += " --secrets " + files.secrets
	}
	fmt.Fprintf(stdout, "Wrote %s.\n\nadmin key: %s\n\nThis is the only time the key is shown: the files hold its hash.\nStart the gateway with\n\n  ai-model-facade serve %s\n\nthen open http://<this host>:8088/amf/ui/ and sign in with the key, to add\nbackends and facades, apply them, and save them to the files.\n", strings.Join(paths, " and "), key, serveArgs)
	return nil
}

// initKeys marks where initConfig takes the admin key's entry, or a note
// that the secrets file holds it.
const initKeys = "  #KEYS\n"

// initConfig is the config init writes.
const initConfig = `# A starting config for ai-model-facade, written by ai-model-facade init.
# Sign in to the admin UI at /amf/ui/ with the key init printed, to add
# backends and facades, apply them, and save them to this file.
# config.example.yaml in the project describes every key.
server:
  # 0.0.0.0 accepts connections from other machines and containers, which
  # every request needs a key from apiKeys for. To keep the admin UI off a
  # network the clients use, give it an address of its own with
  # adminApi.listen, such as 127.0.0.1:8089.
  listen: 0.0.0.0:8088
  #KEYS
  adminApi:
    # admin may change the config over HTTP, and read it and the log.
    write: {allowedKeys: [admin]}
`

// initSecrets heads the secrets file init writes.
const initSecrets = `# The secrets of the config written with it by ai-model-facade init, kept
# apart so the config can be shared: serve reads them with --secrets.
`

// health checks a running gateway. It only parses the config to find
// server.listen; it contacts no backend.
// health reads only server.listen, since the image's health check passes
// no secrets file, and a gateway that is up is healthy whatever else its
// config holds.
func health(files configFiles, stdout, stderr io.Writer) error {
	listen, https, err := config.HealthAddress(files.config)
	if err != nil {
		return err
	}
	url, err := gateway.HealthURL(listen, https)
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
	// One line before anything else serve logs, so a log says which build
	// wrote it even when building the gateway, which lists every backend,
	// takes a while or fails.
	started := []any{"version", currentVersion(), "config", files.config}
	if files.secrets != "" {
		started = append(started, "secrets", files.secrets)
	}
	log.Info("starting", started...)
	g, err := gateway.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	live := gateway.NewLive(g, log, &levelVar, configfile.Files{Config: files.config, Secrets: files.secrets})
	live.KeepLogs(logs)
	live.SetVersion(currentVersion())
	// Listen before logging "listening", so the log line, and anything that
	// waits for it, never comes before the ports are open. A log stream never
	// ends by itself, nor does a status or models stream.
	servers, err := live.Listen(logs.EndStreams, live.EndStreams)
	if err != nil {
		return err
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	a := g.Config.Server.Addresses()
	attrs := []any{"addr", a.Main}
	if a.MainTLS != "" {
		attrs = append(attrs, "https", a.MainTLS)
	}
	if a.Admin != "" {
		attrs = append(attrs, "admin", a.Admin)
	}
	if a.AdminTLS != "" {
		attrs = append(attrs, "adminHttps", a.AdminTLS)
	}
	log.Info("listening", append(attrs, "models", len(g.Models))...)
	for running := true; running; {
		select {
		case err := <-servers.Errs():
			return err
		case <-hup:
			live.ReloadFiles(gateway.SIGHUPCaller) // logs its outcome
		case <-ctx.Done():
			running = false
		}
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := servers.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
