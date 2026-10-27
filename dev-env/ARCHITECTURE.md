# Architecture of the dev environment

**A map of `control` and the files around it: how a request moves through them, and where a change goes.**

This file is for people about to change the environment; [README.md](README.md) describes how to use it.
`control` is a Go module of its own, in `control/`, and shares no code with the gateway, which it never calls.

## Bird's-eye view

The environment is three files, each saying one thing, and one service that acts on them:

- `compose.yaml` says what each container is: its image, volumes and ports, and a profile per backend, so that `docker compose up` starts only `control`, the one service without a profile.
- `config.yaml` says what each backend runs: its models, and the settings its container takes at start.
- `.env` holds what differs between machines: the bind address, ports and models folder. Compose reads it, including on the runs `control` makes; `control` itself doesn't.
- `control` serves the API and does the work.

A request to `control` names a backend.
The API looks it up among the registered backends, reads `config.yaml`, which hands each backend's section to that backend's kind, and asks the configured backend to do the work: through the backend's own HTTP API for its models, its reload and its setup, or through Compose to start and stop it.
Every Compose run merges an override written from `config.yaml`, which is how a container gets its settings.
Work that takes more than a moment runs as a background job, so the `POST` answers `202` at once.

## Code map

All in `control/`, in the order a request reaches them:

- `main.go`: wiring, and the container's mount points (`/work` for `dev-env/`, `/models` for the models store).
- `api.go`: the routes and the rules every endpoint keeps, such as a `POST` that finds its work done answering `200`, and errors as problem details. It knows nothing about any one backend.
- `backend.go`: the registry, and the interfaces a kind implements: `kind`, `configured`, `loader` and `setup`, with `basic` giving each method a default that refuses.
- `config.go`: reading `config.yaml` strictly, giving each section to its kind, and writing the Compose override.
- `compose.go`: running Compose through the host's Docker socket, with `dev-env/`'s host path as the project directory, so the bind mounts resolve on the host.
- `jobs.go`: background work, keyed so that a second request for the same work joins the first.
- `files.go`: downloading the model files and checking each against its sha256, and pruning the ones `config.yaml` no longer lists.
- `manifest.go`: the record, in the models store, of the files `control` downloaded, so a prune removes only those.
- `http.go`: JSON calls to the backends, and their errors.
- `gguf.go`: reading and editing GGUF metadata in place, for Ollama's copies.
- `llamaswap.go`, `ollama.go`: one file per kind of backend.

## Adding a backend

A backend is a Compose service plus a kind, so adding one touches each file that says one thing about it:

1. `compose.yaml`: a service with a profile of its own, publishing its port as `${BIND_ADDRESS:-127.0.0.1}:${<NAME>_PORT:-<port>}`.
2. `.env.example`: that port setting, with its default.
3. `control/<backend>.go`: an `init` that calls `register` with the backend's name, service, profile, address inside the project network, `Health`, the path that answers once it can serve requests, and `NeedsFiles` if it reads the model files; a kind whose `configure` decodes its section; and a configured type that embeds `basic` and overrides only what the backend supports, so everything else answers `409` with a reason.
4. `config.yaml`: the backend's section under `backends`, named as the backend is registered.
5. `README.md`: a section under Backends, and the port in Settings.

## Pitfalls

Mistakes that compile and break something elsewhere:

- **Decoding a section with `Node.Decode`.** yaml.v3 ignores `KnownFields` there, so a misspelled key is silently dropped; `decodeSection` decodes it strictly.
- **A registration whose service or profile isn't in `compose.yaml`.** The override names the service, and Compose rejects an override that defines a service the base file doesn't.
- **A start-time setting written in `compose.yaml`.** An override's `command` or `entrypoint` replaces the base file's, so one in both places is silently ignored in `compose.yaml`; it belongs in the kind's `service`, from `config.yaml`.
- **A backend that reads the model files without `NeedsFiles`.** It starts before they are downloaded.
