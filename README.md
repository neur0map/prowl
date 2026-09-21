# Prowl

[![ci](https://github.com/neur0map/prowl/actions/workflows/ci.yml/badge.svg)](https://github.com/neur0map/prowl/actions/workflows/ci.yml)
[![version](https://img.shields.io/github/v/release/neur0map/prowl?label=version&color=89b4fa)](https://github.com/neur0map/prowl/releases/latest)
[![platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-555)](#install)

Prowl is one local tool for two related jobs:

1. **Code intelligence** - a cited SQLite index of a repository's files,
   symbols, relationships, documentation, and durable project knowledge.
2. **Model routing** - an optional loopback AI gateway that joins API keys and
   subscription accounts into a healthy, explainable model pool.

Run `prowl` with no subcommand to open the unified terminal interface. It shows
project index status, providers, credentials, subscription accounts, models,
routing decisions, activity, setup, and a copyable map of Prowl's commands. No
web dashboard or pre-existing background service is required.

The earlier agent harness that used the `prowl` name is preserved as
`prowl-legacy`. The current product binary is `prowl`; the old `prowl-agent`
command name is retired.

## Quick start

```sh
# Install the current release.
curl -fsSL https://raw.githubusercontent.com/neur0map/prowl/main/install.sh | sh

# Index and configure a project. This performs the first real index.
cd /path/to/project
prowl init

# Open the unified console.
prowl
```

`prowl init` creates or refreshes `.prowl/`, builds lexical, structural, and
semantic indexes, registers the project, and previews the selected harness and
editor integrations before writing them. Re-running it incrementally indexes
what changed; it does not reset project choices.

Non-interactive setup is available for provisioning:

```sh
prowl init --no-input
prowl init --dry-run --integrations auto
prowl init --no-input --integrations cursor,vscode
```

## The unified console

The console has nine tabs:

- **Overview** -- combined estimated token savings across every indexed project,
  gateway readiness, model capacity, recent activity, and next actions.
- **Projects** -- every registered local project with files, symbols, edges,
  semantic progress, last-index time, and estimated token savings. `Enter`
  opens the full status report.
- **Providers** -- production-wired providers only. Filter by access or
  readiness, press `Enter` for details, or `o` for signup.
- **Credentials** -- individual API keys, health, cooldown, and traffic state.
- **Accounts** -- browser/device OAuth for ChatGPT, Claude, Charm Hyper, and
  Copilot, with connection, routing, and published allowance state kept
  separate. Routable accounts add their models automatically after sign-in.
- **Models** -- a compact list of default and named model sets, grouped
  task/access templates, provider checkboxes, full-catalogue search, live
  health, and routing strategy.
- **Activity** -- full-height request and token charts, provider/model usage,
  exact or estimated cost, latency, outcome, route class, and failover attempts.
  Exact, estimated (`~`), and unavailable (`-`) usage remain visibly distinct.
- **Toolkit** - Prowl's major functions with copyable commands.
- **Setup** - safe injection into supported coding harnesses.

Use `Tab` / `Shift+Tab` to move between tabs, arrows or `j`/`k` to move, `/` to
search, and `?` for the complete key guide. The first launch shows a short tip;
each tab supplies one contextual hint without blocking work.

The console leaves terminal mouse capture disabled. Drag normally to select and
copy any rendered text with the terminal's native behavior; no `Shift` bypass is
required. Every console action is keyboard-accessible, and `c` copies the
selected command, path, URL, or code where offered.

Usage and spend remain honest when upstreams differ. Provider-reported totals
win when available; otherwise Prowl combines reported or visibly estimated
tokens with published per-million prices. Missing usage is shown as unavailable,
and models without a published price are unpriced rather than silently free.

## Models and routing are different controls

A **model** is a concrete candidate such as `openai/gpt-5.6-codex` or
`anthropic/claude-sonnet-5`. Enabling a model means Prowl may use it; disabling
it removes it from every automatic route. Selecting a concrete model ID in a
client pins that request to that model.

A **route** decides which eligible model is tried first and how failover is
ordered:

- `auto` uses the active set and the strategy selected in Prowl.
- `auto:smart`, `auto:fast`, `auto:cheap`, `auto:reliable`, and
  `auto:balanced` are explicit per-request overrides. They order the whole
  enabled catalogue by that axis instead of using the active set and strategy.
- `auto:<set>` explicitly uses that named set instead of the active set, while
  retaining the strategy selected in Prowl.

Sets answer **which models may be tried**. The strategy answers **which eligible
model should be tried first for this prompt**. Provider/key health, quota,
cooldowns, and context limits remain hard gates. User choices remain canonical:
smart routing never silently enables a disabled model or escapes the selected
set.

On **Models**, `Enter` edits a set and `Space` activates a set or toggles a
model's membership, depending on context. The set editor initially shows only
that set's selected models; `/` deliberately searches the complete catalogue
when adding another model. `n` opens grouped templates, `s` opens routing
strategies, and `p` opens a checkbox provider picker.

## Subscription accounts and multiple models

Open **Accounts**, select a provider, and press `Enter`. Prowl starts the OAuth
flow and opens the browser automatically. While it is pending:

- `o` reopens the browser;
- `u` copies the authorization URL;
- `c` copies the device code when one exists, otherwise the URL;
- `Esc` cancels the flow.

After sign-in, compatible account models are added to routing automatically.
`Space` then pauses or resumes that durable routing permission; it is not a
second enrollment step. The console reports connection, routing, and allowance
independently, so an allowance-service outage is not presented as a broken
credential and an expired credential explicitly asks for another sign-in.

Model enrollment is not a one-model alias:

- **ChatGPT / Codex** reads the account-scoped Codex catalogue and enrolls every
  visible model returned for that subscription, including its reasoning levels,
  context window, and output limit.
- **Claude Pro / Max** reads Anthropic's account-scoped `/v1/models` catalogue
  with the OAuth credential and enriches current Claude metadata. A curated
  September 2026 multi-model fallback is used only if catalogue discovery is
  temporarily unavailable. Account details show the published five-hour,
  weekly-all-model, and model-scoped weekly windows.
- **Charm Hyper** reads its live OpenAI-shaped catalogue. Its no-charge credits
  endpoint reports the current balance; Hyper grants 100 free credits monthly,
  but does not publish a daily window or an account-specific monthly total and
  reset timestamp.
- **Copilot** sign-in can be stored, but it is not offered as routable capacity

ChatGPT requests use the Codex Responses wire. Claude requests use the native
Anthropic Messages wire, including the Claude Code OAuth headers, request
identity, attestation, and tool-name round trip required by subscription
tokens. These provider inference paths are direct integrations; ACP is not in
the request path.

## Smart prompt routing

Smart routing begins with a bounded local decision, not a routing-model call.
Prowl examines at most 24 KiB from the latest user turn and derives:

- domain: general, coding, agentic/tool use, reasoning, math, research, writing,
  or extraction;
- effort: low, medium, or high;
- signals such as tool use, context size, explicit constraints, and stakes.

System instructions and older conversation turns do not inflate this profile.
The router combines it with:

1. model capability scores for the detected domain;
2. the local catalogue prior when no external score exists;
3. live reliability, latency, cooldown, quota, and context fit;
4. published input/output prices, favoring economical models for simple work
   without allowing price to displace capability for difficult work;
5. the operator's enabled state, set membership, order, and weights.

The decision is exposed rather than hidden. OpenAI-compatible responses include
`X-Prowl-Route-Class`, `X-Prowl-Route-Effort`, `X-Prowl-Route-Reason`, and
`X-Routed-Via`; the same facts appear in Activity and durable request logs.

Research is the one intentional two-stage route. When an automatic request is
classified as research, an enabled Perplexity credential is available, and the
final pool contains a non-Perplexity model, Prowl asks the low-cost
`perplexity/sonar` preset for current, cited findings. It adds those bounded
findings to the final request as explicitly untrusted reference material and
excludes Perplexity from the final model chain. A missing key or failed research
call fails open to the normal route; a client-pinned model never gains an
unexpected second billable call. Successful responses identify this pass with
`X-Prowl-Research-Provider: perplexity`.

This keeps ordinary classification local and fast while making current-source
research explicit. A learned router can still be added behind the same profile
seam when it demonstrates better end-to-end quality and latency on Prowl's own
traffic.

### Live benchmark refresh

Prowl can enrich the catalogue from Artificial Analysis' model API. Supply the
key only to the gateway process:

```sh
export ARTIFICIAL_ANALYSIS_API_KEY='…'
prowl gateway up
```

The gateway fetches the paginated free model endpoint, conservatively matches
external identities to local models, and stores intelligence, coding, agentic,
math, and multilingual scores with provenance and refresh time. It refreshes at
most daily, retries failures after six hours, and retains the last successful
scores on any fetch or parse failure. Without a key, routing continues from the
bundled catalogue and live local observations. The Models tab reports whether
scores are configured, current, stale, or in error.

Benchmark data is attributed in-product to Artificial Analysis. The API key is
read from `ARTIFICIAL_ANALYSIS_API_KEY`; it is never persisted or displayed.

## Use the gateway from coding tools

Start a persistent loopback gateway and inject its canonical `auto` route into
the clients you use:

```sh
prowl gateway up
prowl gateway status
prowl gateway inject omp pi claude codex opencode hermes openclaw prowl-legacy

# Revert only entries Prowl owns.
prowl gateway inject --remove omp pi claude codex opencode hermes openclaw prowl-legacy
prowl gateway down
```

The gateway serves OpenAI-compatible Chat Completions, Responses, legacy
completions, Embeddings, Image Generations, Audio Speech, and Audio
Transcriptions, plus an Anthropic-compatible Messages surface on loopback.
Inference and management routes require a bearer credential. `GET /api/ping`
is the sole unauthenticated
route; it exposes liveness metadata and an optional port-bound proof that lets a
credential-holding client verify the listener before transmitting its token.
The Setup tab configures selected harnesses without displaying credentials.

`prowl gateway` opens the same console as bare `prowl`. On Home, press `d` to
switch **Keep running** on or off. When enabled, closing the console hands the
in-process gateway to a tracked daemon; when disabled, closing a console
attached to that daemon shuts it down safely. `up`, `down`, `status`, and
`restart` remain scriptable lifecycle commands.

### Harness files and portable skills

Gateway injection and skill installation are separate, ledger-backed writes:

```sh
prowl gateway inject pi hermes openclaw prowl-legacy
prowl skills --clients pi,hermes,openclaw,prowl-legacy
```

The custom harness paths are:

| Harness | Gateway provider configuration | Portable Prowl skills |
| --- | --- | --- |
| Pi | `~/.pi/agent/models.json` | `~/.pi/agent/skills/<name>/SKILL.md` |
| Hermes | `~/.hermes/config.yaml` | `~/.hermes/skills/prowl/skills/<name>/SKILL.md` |
| OpenClaw | `~/.openclaw/agents/main/agent/models.json` | `~/.openclaw/skills/prowl/skills/<name>/SKILL.md` |
| Prowl Legacy | `~/.local/share/prowl/prowl.json` | `~/.config/prowl/skills/prowl/<name>/SKILL.md` |

Pi requires each skill directly below its `skills/` directory. Hermes and
OpenClaw discover the grouped recursive trees shown above. Prowl Legacy keeps
the config and data roots it owned before the rename; detection uses the
`prowl-legacy` launcher, never the current `prowl` product binary. Injection
advertises only `auto`, so every harness follows the active set and strategy
selected in Prowl without duplicating Prowl's internal controls. Explicit
`auto:<strategy>` and `auto:<set>` routes remain available to API clients that
intentionally request an override. Re-running either command is safe, and the
corresponding removal path reverts only bytes Prowl still owns.

## Code intelligence

Prowl reindexes changed files before every query and returns cited, bounded
answers. Output is token-lean TOON by default; use `--format human`,
`--format markdown`, or `--json` where needed.

```sh
prowl overview                         # repository map and entrypoints
prowl search "how is auth validated"   # semantic + lexical search
prowl find ValidateToken               # locate a named symbol
prowl def ValidateToken                # read one symbol, not a whole file
prowl outline internal/auth/service.go # file structure without bodies
prowl references ValidateToken         # callers and references
prowl callers internal/auth/service.go # incoming file relationships
prowl impact internal/auth/service.go  # change blast radius
prowl peek internal/auth/service.go:40-90

prowl status                           # index health and saved-token estimate
prowl doctor                           # structural and integration findings
prowl wip                              # recover unfinished local work
prowl changed                          # graph-aware changed surface
prowl history ValidateToken            # symbol history
```

Other built-in surfaces include:

```sh
prowl context search "question"        # bounded context packets
prowl docs add https://example.com/docs
prowl knowledge init                   # reviewed durable project knowledge
prowl review plan                      # bounded large-change review protocol
prowl capabilities search "intent"     # find the right Prowl workflow
prowl serve                            # MCP compatibility server
prowl lsp                              # editor language server
prowl skills                           # install/update agent routing skills
```

The Toolkit tab presents these functions in the console and copies the selected
command with `c` or `Enter`.

## Install

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/neur0map/prowl/main/install.sh | sh
```

Windows amd64 from PowerShell:

```powershell
irm https://raw.githubusercontent.com/neur0map/prowl/main/install.ps1 | iex
```

Both installers select the native `prowl-*` release artifact, verify its SHA-256
checksum, and install it as `prowl`. Build from source with SQLite FTS enabled:

```sh
git clone https://github.com/neur0map/prowl.git
cd prowl
CGO_ENABLED=1 go build -tags sqlite_fts5 -o prowl ./cmd/prowl
```

Update a downloaded build with `prowl update`. Packaged builds defer to their
package manager.

## Integration safety

`prowl init`, `prowl skills`, and gateway injection preview owned writes and use
marker- or ledger-bounded updates. Removal reverts only Prowl-owned material.
Project indexes stay under `.prowl/`; shared state remains under the existing
`prowl-agent` XDG directories for upgrade compatibility.

Prowl is local-first:

- source and index data stay on the machine;
- repository secrets with unambiguous vendor/key shapes are masked before index
  storage;
- external model traffic occurs only when the optional gateway is used;
- benchmark traffic occurs only when `ARTIFICIAL_ANALYSIS_API_KEY` is set;
- configured automatic research traffic may make a Perplexity preflight before
  the final model call, as described above;
- the gateway binds to loopback and authenticates every route except the
  deliberately minimal liveness endpoint.

## Development

The required gate uses SQLite FTS5:

```sh
CGO_ENABLED=1 go test -tags sqlite_fts5 ./...
CGO_ENABLED=1 go vet -tags sqlite_fts5 ./...
CGO_ENABLED=1 go build -tags sqlite_fts5 ./cmd/prowl
bash scripts/onboarding-smoke.sh
```

Plain `go test ./...` is not the project gate because it omits SQLite FTS5.
See [CONTRIBUTING.md](CONTRIBUTING.md), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md),
and [BENCHMARKS.md](BENCHMARKS.md) for implementation and measurement details.
