# ai-model-facade

An OpenAI-compatible gateway that serves facades: model names that exist only in its config.
A client asks for a facade such as `task`; the gateway picks a real model from that facade's candidate list, based on which models its backends have loaded, patches the request with the configured params, and forwards it through a backend adapter (llama-swap, Ollama or Open WebUI).
A facade on Open WebUI also gives a plain OpenAI client Open WebUI's server-side tools: the `openwebui` adapter runs Open WebUI's multi-request chat flow and returns one ordinary response or stream, so its polling, synthesized responses and route field are the point of that adapter, not inconsistencies to remove.

- `README.md` describes what the gateway does, for users.
- `ARCHITECTURE.md` maps the packages for people new to the code, and says where a change goes.
- `dev-env/ARCHITECTURE.md` maps the Docker environment of real backends in `dev-env/`, and says where a change to it goes.
- `docs/agents-design.md` explains why it is built that way, the mechanisms behind each rule below, and the approaches already rejected. Read the section for the code you are changing before changing it.
- `CONTRIBUTING.md` is the short guide for people; its Tests section says where tests go and how they are written, and applies to agents too.
- `config.example.yaml` is a complete example config.

## Working rules

- Make only the changes requested. No whitespace or formatting churn in untouched code. Do not edit or remove comments unrelated to the change.
- Ask a direct question when a requirement is unclear instead of guessing and burying the assumption.
- Keep scope tight: don't add config keys, abstractions or features for hypothetical needs. Extensibility lives in the code's interfaces, not in speculative config.
- Before a change is done, `gofmt -l .` lists nothing and `go vet ./...` and `go test -race ./...` pass; CI runs all three on every push and pull request. If `check`'s output changes, regenerate its golden files with `go test ./cmd/ai-model-facade -update` and review the diff.
- `dev-env/control` is a Go module of its own, so `go vet ./...` and `go test -race ./...` from the root skip it; when changing it, run both in `dev-env/control` too. CI runs them there as well.

## Invariants

These hold everywhere, and the code relies on them.
Each has its reasoning in `docs/agents-design.md`, in the section named after it.

1. **Bodies pass through as raw bytes** (Request bodies). Request and response bodies are never decoded into Go structs or maps: read with `gjson`, edit with `sjson`, forward the bytes. Unknown fields, key order, formatting and number precision survive; only patched keys change. The Open WebUI adapter's synthesized response is the one exception.
2. **Params are merge patches** (Params as merge patches). Every `params` value is applied as JSON Merge Patch (RFC 7386): objects merge recursively, scalars and arrays replace, `null` deletes. `params` may be a map or a list of maps applied in order. Never completely replace a top-level key; that is the llama-swap behaviour this project exists to avoid.
3. **Fixed order of application:** client body, then the facade's `params`, then the winning candidate's `params`, then `model` rewritten to the chosen ID. Gateway params override the client.
4. **Key order is kept** (The walker). Existing body keys stay in place; keys a patch adds are appended to their object in config order. Patches are built from the `yaml.Node` tree, never from a Go map.
5. **Each facade is self-contained** (The concept: facades). No families or other implicit matching by model ID. Shared settings come only from YAML anchors under `vars:`, referenced explicitly.
6. **Never trigger a load the list didn't ask for** (Selection). The winner is the first candidate its backend reports ready; else the first one still starting; else the first `loadIfNone: true` candidate, wherever it sits. Stopping or stopped models never count. An alias has its base model's state; patterns prefer base models over aliases.
7. **Adapters may stream.** An adapter returns a complete response or a chunk stream, and the front end handles both.
8. **Fail only on a wrong config** (Errors and warnings). Unknown keys, an undefined `${ENV}`, a duplicated name, an undefined backend reference, or a selector whose required capability the backend lacks are startup errors. A check that depends on a backend being up and current is a warning (`Gateway.Warnings`): `serve` and reloads log it and start, `check` reports it as an error and exits non-zero.
9. **Selections are observable** (Observability). One log line per request (facade, candidates checked, winner, reason) and the `X-Gateway-Target` response header.
10. **The gateway may be on both sides of Open WebUI** (The concept: facades). A request chain can pass through the same gateway more than once, so:
    - **Loops are per facade** (Routing loops). Each process has a random ID and each hop is `<ID>/<facade>`; the front end refuses (508) a request whose `ai_model_facade_route` already holds its hop, and removes the field. Only the `openwebui` adapter sends it on. The same gateway twice for different facades is not a loop.
    - **One state read per backend per request chain** (Fetching). Every request carries its arrival time (`backend.WithRequestStart`), and a state fetched since then is reused, so a nested request's fetch serves the outer one. `/v1/models` also reads each backend once per listing (`backend.WithStateSnapshot`).
    - **`/v1/models` never waits on a fetch in flight** (Not waiting on itself). It reads state under `backend.WithoutStateWait`, answering from the cache or omitting `current`, because the fetch it would wait on may be its own, looped back through Open WebUI.

## Pitfalls

Mistakes that compile, pass the nearby tests, and break a rule elsewhere:

- **Decoding config with `node.Decode`.** Every config struct goes through `config.DecodeStrict`, or misspelled keys are silently ignored (Config loading).
- **A new adapter without `client.TrackPending(state)`.** Its loads never show as starting, and concurrent requests fall through to `loadIfNone` and cause a swap (Loads the gateway started).
- **Starting goroutines in `Build` or an adapter's `New`.** A failed reload throws that gateway away, and the goroutine leaks; background work starts in `Gateway.start` (Reload).
- **Running a state fetch or Open WebUI cleanup on the request's context.** Concurrent requests share one fetch, so one client leaving would fail them all (Fetching); cleanup runs after the response has ended, when the request's context may already be cancelled (Open WebUI). Both run detached with their own timeout.
- **Putting data into an sjson path unescaped.** Model names such as `qwen3.6` contain path syntax; escape every key segment, including a leading `:` (The walker).
- **Printing or logging from a map in iteration order.** Go randomizes it, so sort first, or `check`'s output and its golden files differ between runs.
- **Passing on another backend header.** Only `Retry-After` is passed on, deliberately; add one through `backend.ChatResponse` only on purpose (Errors).
- **Adding a config key without its docs.** Update the README's config section, `config.example.yaml` where it belongs, and the golden files if `check` prints it.

## Code map

In pipeline order; `ARCHITECTURE.md` has the longer version.

- `cmd/ai-model-facade`: the commands; `run` returns the exit code, so they're tested in-process.
- `internal/config`: parsing, `${ENV}` expansion, strict decoding (`DecodeStrict`, since yaml.v3's `KnownFields(true)` doesn't apply to `yaml.Node.Decode`).
- `internal/gateway`: building and reloading a gateway, and the front end. The front end does HTTP and SSE framing only; selection and rewriting belong to the facade.
- `internal/virtualmodel`: facade types. A new behaviour is a new type with its own schema, validator and handler, never a change to `candidates`.
- `internal/selector`: the selection order (`FirstLoaded`) and how patterns and aliases match.
- `internal/mergepatch`: params compiled from YAML and applied to raw bodies.
- `internal/backend`: the interfaces adapters implement, optional capabilities (`backend.Has`), and the context helpers for sharing state reads.
- `internal/backend/openaihttp`: the shared client and `StateCache`. Rules about reading, caching or adjusting backend state go here, once, not in an adapter.
- `internal/backend/llamaswap`, `ollama`, `openwebui`: one adapter per backend. A new backend is a package here plus a case in `gateway.newBackend`.
- `internal/testkit`: fake backends. Tests live in the package they test; see `CONTRIBUTING.md`.

## Program commands

- `serve <config>` runs the gateway. `SIGHUP` reloads the config, all or nothing: a failed load keeps the running gateway, in-flight requests finish on the gateway that accepted them, and a changed `server.listen` is rejected.
- `check <config>` loads and validates the config, then prints each facade's resolved params per candidate.
- `health <config>` exits 0 if the gateway at `server.listen` answers `/health`; the image's `HEALTHCHECK` uses it. It contacts no backend.

## Code style

The aim is code a reader can follow top to bottom, arriving at one function with no context, and predict what it does without opening other files.

Formatting and case:

- `gofmt`; CI fails on unformatted files.
- Variables and functions: `camelCase`. Types: `PascalCase`.
- Go identifiers keep initialisms uppercase (`APIKey`, `BaseURL`, `ID`, `SSEStream`).
- Arguments in calls and definitions are either all on one line or one per line, never partly wrapped, so adding an argument changes one line.
- YAML config keys are `camelCase` (`apiKey`, `baseUrl`). Request-body keys stay exactly as OpenAI or the backend spells them (`max_tokens`, `chat_template_kwargs`), because the gateway forwards them.

Names and flow:

- A name says what the value is in its context, not how it is stored: `ready`, `catalog`, `winner`, not `flag`, `data`, `result2`.
- Let the type carry the kind: `timeout time.Duration`, not `timeoutDuration`; `models []ModelInfo`, not `modelList`. Prefer a named domain type (`Catalog`, `Selection`) over a bare map or tuple when the value travels.
- Short names only in short scopes (`m` in a three-line loop); the wider the scope, the more the name says.
- Check and return early, so the main path stays unindented. A function that works at two levels, such as parsing a response and deciding what it means, is two functions.

Comments:

- A comment states what the code can't: the constraint being kept, the reason for a shape that isn't obvious, or an outside fact the code depends on, with its source ("llama-swap v263 source, `handleRunning`"). Never the code restated in words.
- A package comment states what the package is for, and lists the steps if it implements a flow. A type comment states what the type represents and what it guarantees. A function comment states what the function guarantees, not its signature.
- Match the surrounding comment density. Don't add comments to code you aren't changing, and don't edit or remove comments unrelated to the change.

Program messages:

- An error or log message names what is wrong, where (config line, model, backend), and what would be right when that is known: `model 'llama3' is not listed by backend 'ollama' and won't count as loaded until it is; did you mean one of llama3:8b, llama3:latest?`
- Names are in single quotes, because double quotes get escaped in logs and JSON bodies.
- Log lines use `slog` key=value attributes, with no field repeating another; never log a secret, log a fingerprint if callers must be told apart.

## Commit messages

The reader knows the code as of the parent commit.

- Subject: what was done, stated plainly, under about 60 characters ("Pass a backend's Retry-After on to the client").
- Body: the situation in which the change matters, then one paragraph per part of the change, each opening with what that part of the code is for. Minor extras go in a final "Also:" paragraph, or, when there are several, an "Also:" list with one bullet per change, continuation lines indented to its text.
- Don't restate what the diff shows, and don't describe how the problem was found.
- Wrap the body at 72 columns.

## Dependencies

Standard library first: `net/http` for server and client, `log/slog`, `path.Match` for candidate patterns.
The only third-party modules are `gopkg.in/yaml.v3`, `github.com/tidwall/gjson` and `github.com/tidwall/sjson`; add nothing else without asking.
No web framework.
The image is a static binary on distroless.
