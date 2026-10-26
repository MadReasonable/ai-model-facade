# Contributing

Contributions are welcome, from a typo fix to a new backend.
This file covers the few things that keep the code consistent, then how the tests work.
The detailed style rules are in [AGENTS.md](AGENTS.md), where coding agents read them; you don't need to learn them first, and a review will point out anything that differs.

## Finding your way

[ARCHITECTURE.md](ARCHITECTURE.md) maps the packages and says where a change usually goes.
[docs/agents-design.md](docs/agents-design.md) explains why each part works the way it does, and which approaches were already tried and rejected, so it's worth a look before changing how a part behaves.

A few design rules hold throughout the code, such as never decoding request bodies into Go types, so that fields the gateway doesn't know reach the backend unchanged.
AGENTS.md lists them and agents-design.md explains them; if a change seems to need breaking one, open an issue first so the approach can be talked through.

## Writing code

- Format with `gofmt`. CI checks it, because unformatted code turns into unrelated whitespace changes in the next person's diff.
- Follow Go's naming conventions and the code around you.
- Give values longer names that say what they are; keep short names like `m` or `ids` to a few lines of code, such as a small loop.
- Comment code that is critical or surprising, saying why it is that way.
- Keep a change to one thing, and leave unrelated code, formatting and comments as they are, so the diff shows only your change.
- The gateway ships as one static binary with three third-party modules; ask before adding a dependency.

## Commits

A commit's subject says what the change does, such as "Pass a backend's Retry-After on to the client".
If the reason isn't obvious from the subject, the body explains it.

Every commit records an author name and email, and both become public once pushed.
To keep your email private, set `git config user.email` to the noreply address GitHub gives you under **Settings > Emails** (`<ID>+<username>@users.noreply.github.com`); GitHub still links those commits to your account.

## Tests

The tests run in under a second, need no network or real backends, and never sleep: every backend is a fake that a test sets up and steps through.
The rest of this file explains how they are organised and written, so a new test lands in the right place and stays fast and deterministic.

### Running the tests

`go test ./...` runs everything.
CI runs `gofmt -l .`, `go vet ./...` and `go test -race ./...` on every push to a branch and on every pull request, so run all three before pushing.
With `-race`, each package takes about a second longer, because the race detector waits a second when a test binary exits.

Test names start with their unit, so `-run` selects an area, for example `go test ./... -run 'TestStateCache_'`.
`-short` skips the one test that starts the program as a process of its own, to send it real signals.

### Where a test goes

Each behaviour is tested once, in the package that implements it.
A test placed higher up, such as a gateway test of a selection rule, repeats coverage and fails far from the cause.

| Package | What its tests cover |
|---|---|
| `internal/backend/openaihttp` | The shared HTTP client and state cache: auth, timeouts, caching, single-flight fetches, pending counts. Adapters build on these and don't test them again. |
| `internal/backend/llamaswap`, `llamaserver`, `ollama`, `openwebui` | What is specific to each adapter: reading its backend's state and model list, its config errors, and, for Open WebUI, the chat flow and the llamaSwap block. |
| `internal/selector` | The selection order and how patterns and aliases match. |
| `internal/gateway` | The HTTP surface and the wiring between the parts, in one file per topic: `auth`, `requests`, `streams`, `selection`, `listing`, `build`, `health`, `reload`. |
| `cmd/ai-model-facade` | The commands, their exit codes and their output. |

Helpers used across one package's test files live in its `helpers_test.go`.

### Naming

A test is named `Test<Unit>_<Behaviour>`, where the behaviour is what happens, stated as a sentence fragment: `TestStateCache_ConcurrentReadsShareOneFetch`, `TestChat_ClientLeavingAStreamStopsTheTask`.
The comment above it states the rule being tested and the reason for it, so a failure says which rule broke.
Variants of one rule are a table of cases run with `t.Run`, each named for its case.

### Fakes

The fake llama-swap, llama-server, Ollama and Open WebUI servers in `internal/testkit` speak the parts of each API the gateway uses, as the versions it was verified against do (llama-swap v263, llama-server b11580, Open WebUI 0.12.3).
A test sets a backend's state and behaviour, such as `SetState("m", "starting")` or a `Reply` with a status and headers, and never writes wire format itself.

Every fake records the requests it receives.
A test checks them with `Count`, `Last` and `Keys`, and calls `Reset` after setup to forget the requests the gateway made while starting.

When a test needs backend behaviour a fake lacks, extend the fake to match what the real backend does, and check the new behaviour in `internal/testkit/testkit_test.go`.
A fake that drifts from its backend lets tests pass against an API that doesn't exist.
To see what a real backend does, [dev-env/](dev-env/README.md) runs one in Docker on demand.

### Waiting without sleeping

A test waits for the event it depends on, never for a length of time.
A sleep is either too short on a busy CI machine, which makes the test flaky, or longer than needed everywhere else, which makes the suite slow.

The tools, by what a test waits for:

- **A request reaching a fake:** `WaitFor(t, "POST /v1/chat/completions", n)`.
- **A load or chat to proceed:** `HoldLoads` returns a `Gate` that holds every load until `Open`; an Open WebUI `Chat` takes a `PollGate`, which lets one poll through per value sent, and a `StopGate`.
- **Readers waiting on the state cache:** the cache's unexported `waiting` hook, through `readersWaiting` in the `openaihttp` tests.
- **A log line in a gateway test:** `gw.logs.waitFor(t, "...")`.

Every wait gives up after `testkit.Timeout`, 5 seconds, so a test that waits for something that never happens fails instead of hanging.

A test of a timeout measures an upper bound on how long the call took, with the timeout set to a fraction of a second.
A test that sends a body the gateway refuses unread calls the handler directly with `gw.direct`, because over a connection Go's HTTP server waits 500 ms before closing it.

### Golden files

The full output of `check` is compared with files in `cmd/ai-model-facade/testdata`.
After a change to that output, regenerate them with `go test ./cmd/ai-model-facade -update` and review the diff before committing it: the diff is the change a user will see.
