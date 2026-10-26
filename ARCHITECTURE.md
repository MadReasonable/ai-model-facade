# Architecture

**A map of the code: what each package owns, how a request moves through them, and where a change goes.**

This file is for people about to read or change the code.
The [README](README.md) describes what the gateway does.
[docs/agents-design.md](docs/agents-design.md) is the detailed account, written for coding agents: the reasoning behind each rule, the mechanisms that keep it, what was verified against which backend version, and the approaches already rejected.
Read it too before changing a part whose reasons this file doesn't give.

## Bird's-eye view

The gateway is an OpenAI-compatible HTTP server whose model names are facades: entries in its config, each with an ordered list of candidate models on one or more backends.
A request for a facade passes through five stages, each in its own package:

```
client -> front end -> facade -> selector -> params -> backend adapter -> backend
          gateway    virtualmodel selector  mergepatch  backend/...
```

The front end parses nothing but what it must, the facade decides the policy, the selector picks one model from the backends' current state, the params are patched into the request as byte edits, and the adapter forwards it and hands back a response or a stream.

Two rules shape every package.
Request bodies stay raw bytes from end to end: values are read with gjson and edited with sjson, never decoded into Go types, so whatever the gateway doesn't patch reaches the backend unchanged.
And a facade never causes a model load its candidate list didn't ask for, which is why selection reads backend state before it chooses.

## Code map

### `cmd/ai-model-facade`

The commands: `serve`, `check`, `health` and `version`.
`run` takes the arguments and output writers and returns the exit code, so the commands are tested in-process.
`serve` binds the port, serves a `gateway.Live`, and reloads it on `SIGHUP`.

### `internal/config`

Turns the YAML file into a `Config`: parse to a `yaml.Node` tree, expand `${NAME}` in string values, decode strictly.
`DecodeStrict` rejects unknown keys, which yaml.v3 doesn't do for node trees.
Backend and facade entries stay as nodes, because each type decodes its own schema.

### `internal/gateway`

Builds a running gateway from a config (`Build`) and serves it.
`Build` creates every backend, links the ones that refer to others, lists every backend's models once, builds every facade, and runs the checks against those lists; problems that depend on a backend being up become `Gateway.Warnings` rather than errors.
`Live` holds the current gateway and swaps in a new one on reload, only if it builds.
`server.go` is the front end: the OpenAI endpoints, auth, the routing-loop check, the selection log line, and SSE relay. It does framing only; anything about choosing or rewriting belongs to the facade.

### `internal/virtualmodel`

Facade types, each with its own schema, validator and `Handler`.
`candidates`, the only type and the default, compiles the facade's and each candidate's params, checks candidate IDs against their backends, and on each request runs the selector, applies params and calls the winner's backend.
`Handler.Current` predicts the selection without making a request, for `current` in `/v1/models`.
A new behaviour is a new type here, so existing config entries never change meaning.

### `internal/selector`

`FirstLoaded` picks the first ready candidate, else the first starting one, else the first `loadIfNone` one, sending a backend that is loading a model nothing it would refuse.
It reads each backend's state concurrently and waits for a backend only when the walk reaches one of its candidates.
A selector declares the backend capabilities it `Requires`, and config loading checks them.

### `internal/mergepatch`

Compiles `params` from YAML nodes into JSON Merge Patches (`Compile`) and applies them to raw bodies (`Patch.Apply`).
It walks the patch, not the body, so keys the config doesn't mention are never touched, and added keys follow config order.

### `internal/backend`

The contracts between the gateway and its adapters.
`Backend` (`ListModels`, `Chat`) is what every adapter implements; `ModelStateProvider` (`LoadedModels`) is an optional capability, found through `backend.Has`.
`ChatResult` carries either a complete `ChatResponse` or a `ChatStream`, and `SSEStream` parses a backend's event stream.
Context helpers control how state reads are shared within one request or one model listing, so a backend's state is read once even when a request loops back through Open WebUI.

### `internal/backend/openaihttp`

What adapters share: `Client` (HTTP with timeouts, auth and passthrough, chat forwarding) and `StateCache`.
`StateCache` is where state reads are cached, shared between concurrent requests, and adjusted for loads the gateway started itself (`Pending`).
Rules about backend state are implemented here once, not in each adapter.

### `internal/backend/llamaswap`, `llamaserver`, `ollama`, `openwebui`

One adapter per kind of backend.
`llamaswap`, `llamaserver` and `ollama` read their backend's state and forward requests unchanged.
`openwebui` runs Open WebUI's own chat flow and builds the OpenAI response itself, the one place a response is synthesized; it can also take llama-swap models' state from a `llamaSwap` backend (`Link`).
A new kind of backend is a new package here, built on `openaihttp`, plus one case in `gateway.newBackend`.

### `internal/testkit`

Fake llama-swap, llama-server, Ollama and Open WebUI servers that record what they receive and let a test hold a load or a poll.
[CONTRIBUTING.md](CONTRIBUTING.md) says how tests use them.

## Where a change goes

- A new config option for a backend: its adapter's `Config`, or `openaihttp.Config` if every adapter takes it.
- A new rule about which model wins: the selector, or a new facade type if it isn't load-aware selection.
- A new rule about how backend state is read or reused: `openaihttp.StateCache`.
- A new endpoint or response header: `internal/gateway/server.go`.
- A new kind of backend: a package under `internal/backend`, and a fake in `internal/testkit` for its tests.
