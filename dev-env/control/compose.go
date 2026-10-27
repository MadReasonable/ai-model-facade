package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// compose runs Docker Compose on the environment's compose.yaml, through the
// host's Docker socket.
//
// Compose reads compose.yaml and .env in this container, where dev-env/ is
// mounted at workDir, but the bind mounts it asks for are made by Docker on
// the host, so relative paths such as ./backend-config/llama-swap.yaml must
// resolve to host paths. So every run names the host path of dev-env/ as the
// project directory, which this container finds out by asking Docker where
// workDir is mounted from.
//
// Every run also merges an override that hands the config's settings to the
// containers (config.override), written to a file of its own for that run,
// so concurrent runs never share one and each sees the config as it is now.
type compose struct {
	workDir  string // where dev-env/ is mounted in this container
	hostDir  string // the same folder on the host
	override func() ([]byte, error)
}

func newCompose(ctx context.Context, workDir string) (*compose, error) {
	self, err := os.Hostname() // Docker names a container's hostname after its ID
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", self).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect '%s': %w", self, errorOutput(err))
	}
	var mounts []struct{ Source, Destination string }
	if err := json.Unmarshal(out, &mounts); err != nil {
		return nil, err
	}
	for _, m := range mounts {
		if m.Destination == workDir {
			return &compose{workDir: workDir, hostDir: m.Source}, nil
		}
	}
	return nil, fmt.Errorf("container '%s' has nothing mounted at '%s'; mount dev-env/ there", self, workDir)
}

// run runs `docker compose <args>` and returns its standard output.
func (c *compose) run(ctx context.Context, args ...string) ([]byte, error) {
	override, err := c.override()
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "override-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(override)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	base := []string{"compose", "--project-directory", c.hostDir, "--file", filepath.Join(c.workDir, "compose.yaml"), "--file", f.Name()}
	// Compose looks for .env in the project directory, which is a host path
	// this container doesn't have.
	if env := filepath.Join(c.workDir, ".env"); exists(env) {
		base = append(base, "--env-file", env)
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// state returns the service's container state as Docker reports it, such as
// running or exited, or "absent" if it has no container.
func (c *compose) state(ctx context.Context, service string) (string, error) {
	out, err := c.run(ctx, "ps", "--all", "--format", "json", service)
	if err != nil {
		return "", err
	}
	// One JSON object per line; no lines when the container doesn't exist.
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ps struct{ Service, State string }
		if err := json.Unmarshal(line, &ps); err != nil {
			return "", err
		}
		if ps.Service == service {
			return ps.State, nil
		}
	}
	return "absent", nil
}

func (c *compose) start(ctx context.Context, profile, service string) error {
	_, err := c.run(ctx, "--profile", profile, "up", "--detach", service)
	return err
}

// build builds the image of a service that compose.yaml builds, from
// folder under dev-env/. Compose run here would read the build context at
// its host path, which this container doesn't have, so control builds from
// its own mount and tags the image with the name compose.yaml gives it;
// `up` then uses it. Docker's build cache makes a rebuild with no changes
// quick.
func (c *compose) build(ctx context.Context, folder, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "--quiet", "--tag", image, filepath.Join(c.workDir, folder))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker build '%s': %w: %s", folder, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stop stops the backends and removes their containers, in one Compose run,
// which stops them in parallel. A stopped container would keep naming the
// project network, which `docker compose down` removes once control's
// container is gone, so Compose would start it again on a network that no
// longer exists. What a backend keeps is in named volumes, which a down
// without --volumes leaves alone. The services are named, since a bare down
// would also remove control and the network.
func (c *compose) stop(ctx context.Context, bs ...backend) error {
	var profiles, services []string
	for _, b := range bs {
		profiles = append(profiles, "--profile", b.Profile)
		services = append(services, b.Service)
	}
	_, err := c.run(ctx, append(append(profiles, "down"), services...)...)
	return err
}

// containerID returns the ID of the service's container, "" if it has none.
func (c *compose) containerID(ctx context.Context, service string) (string, error) {
	out, err := c.run(ctx, "ps", "--quiet", service)
	return strings.TrimSpace(string(out)), err
}

// logs returns everything the service's container has logged.
func (c *compose) logs(ctx context.Context, profile, service string) ([]byte, error) {
	return c.run(ctx, "--profile", profile, "logs", "--no-log-prefix", service)
}

// signal sends a Unix signal, such as SIGHUP, to the service's container.
func (c *compose) signal(ctx context.Context, profile, service, signal string) error {
	_, err := c.run(ctx, "--profile", profile, "kill", "--signal", signal, service)
	return err
}

// errorOutput adds a failed command's standard error to its error.
func errorOutput(err error) error {
	if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return err
}
