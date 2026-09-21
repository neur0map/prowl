# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[semantic versioning](https://semver.org/).

## [Unreleased]

### Added
- New **Efficient** routing strategy: it classifies each prompt and caps the
  candidate pool at the capability tier the difficulty warrants (high stakes
  lift the cap one tier), then orders within the cap like Smartest. A simple
  prompt can no longer reach a frontier/most-expensive model, which fixes the
  case where an unpriced subscription pool sent trivial work to the top model on
  tier alone. Opt-in per set (`auto:efficient` also works); existing strategies
  are unchanged.
- Model listings show each model's intelligence rank (the `Intel` column, e.g.
  `frontier #5`), so it is clear why the router prefers one model over another.
- Strategy names in the console are clearer: `smartest` now reads **Smartest**
  (was the confusing "Smart routing"), alongside Efficient, Balanced, Fastest,
  Most reliable and Priority, each with a one-line description.
- Every push to `main` now cuts a stable release, not just `unstable`: the
  release workflow bumps the patch on each push (`0.15.x`), rolls the minor on
  the ninth bump (`0.15.8` -> `0.16.0`), builds all five targets, and publishes
  `vX.Y.Z` plus the rolling `stable` channel that `prowl update` follows. A
  green gate (gofmt, vet, `go test`) runs on the exact commit and blocks any red
  push from publishing.
- The console is reworked around the two things an operator actually does:
  build routing sets and connect providers. A grouped sidebar replaces the
  nine-tab strip (Home; Gateway: Routing, Providers, Activity; Workspace:
  Projects, Setup, Toolkit), with `1`-`7` jumping to a section and six rows of
  chrome instead of eleven. **Routing** opens on your sets - each with its
  model count, its own strategy and whether it is active - and the presets
  (Deep work, Coding, Quick chores, ...) that create a ready-made set in one
  keystroke. Opening a set is where models are chosen: only models from
  connected providers are offered, providers start collapsed, `space` takes a
  model or a whole provider in or out, `f` filters by membership, provider,
  access and tier, and `/` searches. Every set carries its own routing
  strategy (`r`), and presets bring a fitting one (Quick chores is fastest
  first, Deep work smartest first). The legacy global fallback chain is no
  longer shown as a "built-in" set. **Providers** merges the old Providers,
  Credentials and Accounts tabs: connected providers lead, the rest of the
  directory follows, and `c` connects by API key, keyless enable or browser
  sign-in according to the provider, `space` pauses or resumes, `d`
  disconnects, and `enter` opens a manage panel with every credential, its
  health and traffic, the account allowance, and per-credential check, reveal,
  enable, cooldown and removal actions. Sign-in platforms without a routing
  adapter (Copilot) are no longer listed as accounts. Keys live only in the
  footer; there is no second control bar above a table.
- Home leads with subscription capacity: one bar of remaining allowance
  across the signed-in accounts (100% counting down), labelled with the token
  total inferred LiteLLM-style from what this gateway routed against each
  account's own used-percent, then each account's windows (Claude 5h/7d,
  ChatGPT/Codex 5h/weekly from the same usage endpoint `codex status` reads,
  Hyper credits) with mini bars and reset times. `GET /api/logins/usage` now
  reports a ChatGPT (Codex) account and, per window, `windowSeconds` and
  `tokensUsed`.
- Keyless providers (Kilo Code, OVHcloud, AI Horde) take an optional key: `c`
  opens the key form, an empty key enables the free tier and a real key
  unlocks the provider's paid models. Destructive confirms default to the
  action the operator chose, so `enter` disconnects instead of silently
  keeping. The set editor gains `A` to select or clear every model shown.
- Sets carry a per-set routing strategy: `profiles.strategy` (empty inherits
  the console-wide default), `POST`/`PATCH /api/profiles` accept `strategy`,
  preset rows advertise theirs, and `ResolveChain` orders a profile's chain by
  the profile's own strategy for `auto` and `auto:<set>` requests.
- Every table is a flat grid with a one-line search box, small-caps headers, a
  range line and centred empty states; stat strips lost their boxes; pickers
  and filter panels share one design. The help overlay documents the new verbs.
- Home can rotate the unified inference key: `g` regenerates it behind a
  confirm, the previous key stops authenticating the instant the new one is
  stored, and the fresh key is revealed so it can be copied and re-injected.
- Prowl is now released under the MIT License, with a `NOTICE.md` that credits
  the projects it builds on: FreeLLMAPI (the gateway is a Go port of it),
  LiteLLM (provider directory data), Oh My Pi (a supported harness), and Kilo
  Code (a bundled keyless provider).

### Fixed
- Claude models routed through the gateway are no longer far weaker than calling
  them directly. A client's `reasoning_effort` now becomes Claude **extended
  thinking** (`thinking` with a budget scaled to the effort and capped below
  `max_tokens`), instead of `output_config.effort` - which Haiku rejected
  outright and which never engaged Opus's reasoning, so Opus ran as the
  non-thinking model and repeated work. Conflicting sampling params
  (`temperature`/`top_p`/`top_k`) are dropped when thinking is on, since
  Anthropic rejects them. Discovered Claude models are also now marked
  reasoning-capable, so their reasoning request is not stripped before the
  transform can turn it into a thinking budget.
- The gateway forwards a harness's request faithfully: it never strips a param,
  drops the system prompt, or truncates context. It routes to the chosen model
  and provider and passes everything through, translating only the wire protocol
  to reach that provider. (This replaces an earlier attempt that dropped
  reasoning params for models flagged non-reasoning, which mis-flagged Claude
  and silently weakened it.)
- Unmeasured keys now rotate fairly across a model's key pool. The round-robin
  base was rotating over a key order the exploration draw had already
  scrambled, so load was not actually spread and the behaviour was
  nondeterministic; the rotation now runs over a canonical key order.
- `prowl update` downloads the build for the platform it is running on
  (`prowl-<os>-<arch>`, `.exe` on Windows) instead of always fetching the
  linux-amd64 binary, which previously overwrote a macOS, Windows or arm64
  executable with the wrong architecture. A platform with no published build is
  refused with a clear message rather than a broken download.
- Selecting or clearing every model in a large set no longer times out: the
  set-write enables its member models inside the reorder transaction instead of
  one HTTP request per model, so a bulk select over a catalogue of hundreds
  completes in a single round trip.
- A curated named set is no longer flooded with an account's whole catalogue on
  every restart: newly enrolled login models auto-join only the Default
  (auto-include) profile and the global fallback chain, never whichever
  hand-picked set happens to be active.
- `/api/models` no longer reports every keyless provider's models as available:
  availability now matches the router (an adapter plus an enabled, non-errored
  credential row, which a keyless provider gets when it is switched on) and
  honours a retired row's `available` flag, so the Models page stops offering
  models nothing can route to.
- An install that enrolled a subscription before login models carried their
  own endpoint scope listed every account model twice; the next authoritative
  discovery now retires the legacy unscoped rows.
- The canonical source repository and Go module are now
  `github.com/neur0map/prowl`; installer, release, badge, and plugin metadata use
  the same product identity while existing `prowl-agent` state directories stay
  unchanged for upgrade compatibility.
- The product binary is now `prowl`; the earlier agent harness that owned that
  name is preserved as `prowl-legacy`, and the transitional `prowl-agent`
  command is retired. Running `prowl` with no arguments opens one nine-tab
  terminal console for project status, indexed projects, providers,
  credentials, subscription accounts, models and routing, activity, the code
  toolkit, and setup. `prowl init` remains the direct indexing path. The console
  leaves mouse capture disabled so native terminal selection and copy work
  without a modifier. Home adds a **Keep running** switch that promotes the
  session gateway to a tracked daemon on exit or safely stops an attached daemon
  when switched off.
- Smart routing now classifies only the latest user turn locally by domain and
  effort before applying hard capability constraints, operator controls,
  benchmark fit, live health, and an economy score from published input/output
  prices. It does not spend a model call merely to choose a model. Research is
  the explicit exception: an automatic research request can use low-cost
  Perplexity Sonar to collect current cited findings, then hand bounded,
  untrusted context to a non-Perplexity final route. Pinned requests never gain
  the extra call, and preflight failure falls back to the normal chain. Route
  class, effort, scoring reason, and research provider are exposed in response
  headers and the durable activity trail. Artificial Analysis benchmark scores
  retain their bounded refresh and last-known-good behavior.
- Subscription sign-in now opens the authorization page automatically, keeps
  the URL and device code copyable, and adds every discovered account model to
  routing without a separate enrollment step. Connection, routing, and
  allowance state are reported independently, including actionable re-sign-in
  state instead of falsely marking scrubbed credential metadata unreadable.
  Claude uses Anthropic's native Messages wire protocol with OAuth attestation
  and Claude-compatible streaming/tool translation; ChatGPT/Codex keeps its
  native subscription transport and complete dynamic model list.
- Home now includes a compact 24-hour request graph with spend, token, and
  request totals. Activity uses the full terminal canvas for request trends,
  model-share bars, and recent calls. Exact, estimated (`~`), and unavailable
  (`-`) token usage stay distinct. Provider-reported costs take precedence;
  otherwise Prowl estimates spend from the live model catalogue's per-million
  prices, and an unpublished price remains unavailable instead of becoming a
  false `$0.00`.
- The Overview still leads with combined estimated token savings across all
  indexed projects. Models opens on a compact default/named-set list, groups
  templates by task and access, starts editors with active providers, provides
  a checkbox provider modal, searches the full catalogue with `/`, uses `Space`
  consistently for membership/activation, and makes custom-set removal work
  through both rendered actions and keyboard confirmation.
- Added the gateway: a terminal-native provider and routing management plane.
  Its engine provides smart strategy ordering, per-key failover, quota windows,
  cooldown benching, an encrypted key vault, the provider/model directory, and
  OpenAI-, Anthropic-, and Responses-compatible `/v1` routes, including chat,
  embeddings, image generation, speech synthesis, and transcription. Durable
  gateway state remains under `$XDG_DATA_HOME/prowl-agent/gateway` for upgrade
  compatibility. The unified Prowl console manages providers, keys and custom
  endpoints; Claude, ChatGPT, Copilot, and Charm Hyper subscription accounts;
  named sets and strategies; activity; and routing-only injection into OMP, Pi,
  Claude Code, Codex, OpenCode, Hermes, OpenClaw, and the preserved Prowl Legacy
  harness. Injection is ledger-backed so removal restores displaced values,
  protects credential files, and preserves later user edits. Scriptable
  lifecycle commands remain available.
- Added an experimental native code-review subsystem (`prowl review`:
  `plan`, `unit`, `check`). It captures exact review churn against a merge base,
  resolves immutable review scopes, persists bounded review units and
  dependency-ordered plans from a sandboxed local Git reader, and verifies
  review coverage and freshness. Ships the review protocol for agents plus a
  blind control/treatment evaluation harness (`internal/review`,
  `internal/revieweval`, `cmd/review-eval*`).
- Added an `issue-resolution` agent skill. It routes a "work through the open
  GitHub issues" task through Prowl for investigation (search, find, def,
  references, impact) and `gh` for the issue API, then closes each issue with a
  short, human-toned note. Installed alongside the other skills via
  `prowl skills`.
- `prowl skills` gained a non-interactive apply path and explicit client
  selection. `--yes` applies the reviewed plan without a prompt, the only way a
  piped or provisioning run writes; a plain non-interactive run remains a
  preview. Pi installs each portable skill directly at
  `~/.pi/agent/skills/<name>/SKILL.md`. Hermes uses the recursive
  `~/.hermes/skills/prowl` tree, OpenClaw uses `~/.openclaw/skills/prowl`, and
  Prowl Legacy retains `~/.config/prowl/skills/prowl`. Detection recognizes
  their home directories or explicit launchers, including `prowl-legacy`
  without mistaking the current `prowl` product for a harness.
- Packaged builds no longer self-update. A build-time `main.managedBy` stamp
  (`-ldflags "-X main.managedBy=pacman"`), or a running executable whose install
  directory the user cannot write, makes `prowl update` print that the
  binary is managed by the package manager (on Ryoku: `ryoku update`) and exit 0
  without downloading. `version` and `status` still report update availability.
- Added a CLI-first adoption package for Claude and OMP. `prowl skills`
  previews release-matched user assets, defaults to No, never writes from a
  non-interactive session, refuses foreign-file conflicts, and tracks each owned
  file for safe updates and removals. The installed `code-search` skill,
  Claude `/prowl:search` command, and bounded fail-open advisories steer
  structural repository questions to the low-overhead Prowl CLI while leaving
  exact literal and filename work to native tools. `doctor --integrations`
  reports CLI and asset health plus required restart or reload actions. A blind
  control/treatment harness now measures Prowl-first routing, broad-search
  reduction, grounded correctness, and native control behavior across both
  clients; measured rates are retained in its generated reports rather than
  claimed here. MCP remains available as an optional compatibility transport.
- Secrets are masked before they are stored, not filtered on the way out. prowl
  indexes whatever a repository contains, credentials committed in source
  included, and every retrieval path emits stored text into an agent's context,
  so masking at storage time is the only way the on-disk index -- chunk text, the
  FTS index, and the vectors -- never holds a cleartext secret. Five sinks are
  masked: chunk text, symbol signatures, doc comments, resource values, and raw
  dependency-edge text. This rewrites content on the next index of an existing
  repository: a matched value becomes `[redacted]` in place, and because masking
  is destructive and happens before storage there is no cleartext copy left to
  recover -- the only way to restore a value is to remove the credential from
  source and reindex. Masking is deliberately precise, covering only shapes
  unambiguous enough to carry no false positives: provider-prefixed API keys (AWS,
  GitHub, Slack, Google, Stripe, OpenAI/Anthropic), JWTs, the password field of a
  URL userinfo section (including the username-less `redis://:pass@host` form),
  and PEM private-key bodies in any textual embedding -- a multi-line block, or a
  one-line `.env`/JSON/YAML value that carries the key with literal `\n` escapes.
  Two things are deliberately NOT masked, because catching them would mean
  destroying real source. A homegrown credential with no vendor prefix -- a
  random-looking value assigned to a secret-named variable, `API_KEY = "8f3k..."`
  -- is left alone: a generic entropy/character-class heuristic was measured
  masking ordinary camelCase identifiers at six real sites in this repository, and
  destructive masking cannot afford a false positive. And a whole private key
  re-encoded as a single opaque base64 blob with no BEGIN/END marker is not masked,
  because detecting it would mean decoding arbitrary base64 and masking anything
  key-shaped. Unreliable detection belongs in a non-destructive warning, never in
  destructive masking.
- `doctor` reports a `hardcoded_secret` finding, at `warn`, for each file that had
  a value masked. The count comes from the per-file `files.redacted` column
  written at index time -- durable evidence a secret was removed, independent of
  the (now masked) chunk text -- so a repository that reported clean before will
  show new `warn` findings once it is reindexed with this release, one per file
  that contained a matched credential.
- Doc comments are extracted, masked, and indexed as their own field, so a file
  whose docstring answers a query surfaces even when its code shares none of the
  query terms. This is a recall fix, not a reranking: doc-answer matches are
  appended below the existing code results and never reorder them. Measured
  example: `search "keep secrets out of the log file"` returned `agent/redact.py`
  nowhere in 50 results before, and now returns it at rank 11 -- immediately below
  an unchanged, byte-identical top ten of code matches. Up to three doc answers are
  inserted starting at rank 11, which shifts the results below them down by up to
  three slots, so on a full page (`--limit 50`) a weak match near the very bottom
  can be traded off the page for a doc answer surfaced at rank 11; the top ten and
  the read window above the insertion never move. This recall applies to the
  CLI `search` and `--smart` commands and to the legacy MCP tools `similar_code`
  and `smart_search`. It does NOT apply to the core MCP surface's `search_context`
  tool, which retrieves over chunk text only, so an agent on `--mcp-surface core`
  does not get doc-comment recall.
- `outline` (and the `outline` MCP tool) now states each symbol's purpose in one
  sentence drawn from its doc comment, so a file's shape reads as intent and not
  signatures alone; struct field comments no longer leak into the struct's own
  signature.
- `span <symbol>` (CLI and the `span` MCP tool) returns a symbol's current file
  and line range plus a content digest, so an agent can tell whether the body it
  planned to edit has drifted before editing against a stale range. The digest
  hashes the symbol's current body bytes, not its position, so a symbol that moves
  without changing keeps the same digest.
- `history <symbol>` (CLI and the `history` MCP tool) returns the commits that
  touched a symbol, newest first, from `git log -L` over the symbol's current line
  range. Because `git log -L` cannot be combined with `--follow`, a symbol whose
  file was renamed has its history truncated at the rename -- commits from before
  the file's current path are not reached.
- A documented versioning standard with two release channels
  (`docs/VERSIONING.md`). `var version` in `cmd/prowl-agent/main.go` is the one
  source of truth: every push to `unstable` advances the patch, and the tenth bump
  rolls into the minor (`0.9.9` then `0.10.0`), with the major left manual.
  **stable** is fed by `main` and is what `prowl update` and the installers
  use; each release is permanent under its own `vX.Y.Z` tag with a rolling
  `stable` tag pointing at the newest. **preview** is fed by `unstable`, carries
  every commit as it lands, and is opt-in with `PROWL_UPDATE_CHANNEL=preview`.
  Both publish all five targets with checksums. The `nightly` tag is retired and
  kept in step with stable so binaries released before the split keep updating.
- Enabling AI no longer requires a local model. `init` now resolves the
  semantic-assist backend up front: it prefers Ollama when installed, otherwise
  it borrows an installed coding-agent CLI (reranking only) instead of walking
  you through Ollama setup, so the AI toggle is meaningful with no local model.
  The interactive wizard adds a backend step (local model vs coding agent) when
  both are viable, and states the tradeoff: embeddings (search by meaning) need
  the local model; the agent only reranks. Force a backend non-interactively
  with `--ai-provider ollama|agent` / `--ai-command`.
  Reranking is a lightweight ordering task, not coding, so prowl pins the
  agent's cheapest tier by default (`claude -p --model haiku`,
  `omp -p --model haiku`, `codex exec -m gpt-5-mini`); override with
  `--ai-command`. The agent backend reranks only; it has no embedding endpoint,
  so vector search still needs a local embed model. Complements the MCP-sampling
  path (which the 2026-07-28 RC deprecates and which only works over MCP with
  capable clients, where the client's own model reranks in-process).
- `sketch` (and the `sketch_ui` MCP tool) renders how a UI looks and behaves
  from source, without a screenshot or a runtime, across four dialects. QML and
  React (jsx/tsx) render as an element tree with each element's visual properties
  (layout, color, text), behavior (handlers, animations, QML conditional
  visibility, and every React conditional-return branch), and declared
  properties. A Go/lipgloss TUI renders as
  the color palette (with hex constants resolved) and named styles with their
  attributes; CSS/SCSS renders as its design tokens (custom properties and SCSS
  variables) and rules. QML token references are resolved against the project's singletons,
  so `color=Tokens.ink` reads as `Tokens.ink⟨#cdd6f4⟩` (alias chains followed,
  the nearest same-named singleton preferred). The argument is a component name
  (resolved through the index) or a file path; text by default, `--json` for the
  structured model. This answers
  "how does this screen look" -- the hardest thing to convey to an agent in
  plain text -- in prowl's currency: deterministic, cited, token-lean.
- Always-on project map in `AGENTS.md`. `init` seeds, and every `overview`
  refreshes, a compact map block (size, languages, subsystems, entrypoints,
  central files, guides) so a coding agent reasons from current, passive context
  by default instead of guessing or grepping. Written only when the repo already
  opted into Prowl, never clobbering, and excluded from the index.
- `graph` writes a self-contained interactive HTML dependency graph (files as
  nodes colored by subsystem and sized by dependents, resolved edges as links,
  vanilla-JS force layout with zoom/pan/drag/search). No external assets; opens
  offline; a visual map to hand a human or drop in a review.
- `bench` measures token efficiency with zero external services: per question,
  the tokens in Prowl's cited packet vs reading the cited files whole vs the
  whole repo. On this repo, cited packets are ~97% smaller than the files they
  cite. See `BENCHMARKS.md`.
- `doctor` gains `--format sarif` (GitHub code scanning), `--format shields`
  (badge endpoint), `--baseline` (report only findings new since a prior report),
  and `--fail-on` (CI gate). A composite GitHub Action and example workflow run
  doctor on every pull request and upload SARIF, surfacing new issues inline.

- The bundled code embedder ships int8 instead of float32, taking the binary from
  118.4 MB to **72.9 MB** with no measured retrieval cost. A static embedding is the
  weighted mean of its tokens' rows followed by L2 normalization, so per-element
  quantization error is zero-mean, cancels across a chunk's tokens, and loses its
  residual scale to normalization. Measured over 20,000 real source chunks and 10
  natural-language queries: mean cos(fp32, int8) = 0.99998 (worst 0.99992),
  overlap@10 = 100% on every query, overlap@50 = 99.0%; re-indexing an 8.5k-file
  repo returns byte-identical top-3 results. Each row carries its own symmetric
  scale, folded into the pooling weight so the inner loop is unchanged -- int8
  costs no throughput and saves the same 45 MB of resident memory. The upstream
  project already distributes these models at roughly one byte per parameter;
  shipping float32 was our own overhead. `cmd/quantize-embed-model` regenerates the
  blob so the committed artifact has reproducible provenance.
- `knowledge lint --repair` re-points anchors whose code moved. A note anchored to
  a line range used to report `stale_anchor` the moment anything was inserted
  above it, so ordinary refactoring decayed accurate evidence into a warning
  nobody acted on, and retrieval cited coordinates that no longer held the
  anchored lines. Lint now separates code that *moved* from code that *changed*:
  a new `knowledge.moved_anchor` finding, at `info`, names the range the anchored
  lines occupy now, retrieval and the indexed anchor rows cite that range, and
  `--repair` writes it back to the document while preserving frontmatter prowl
  does not own. `stale_anchor` is now reserved for the anchored lines actually
  changing, and `--repair` refuses to touch those, so relocation can never
  manufacture evidence for a claim the code stopped supporting. Relocation needs
  no new stored state: it re-uses the region hash already recorded, searching
  outward from the recorded position so the nearest match wins and a repeated
  region resolves deterministically, under a byte budget that reports
  `stale_anchor` rather than stalling a lint pass. A symbol anchor whose symbol
  was renamed but whose body is untouched is recovered the same way.
- A guard keeps prowl's agent-facing text honest against its own command tree.
  Every `prowl ...` invocation in the capability manifests, the installed
  skills, and the injected project map is now resolved against the registered
  cobra commands, with cited flags checked too, and every capability `tools:` name
  against the registered MCP tools. The previous check only asserted a
  `prowl ` prefix, so a renamed command would ship while three surfaces kept
  telling agents to run something that exits non-zero. It walks the command tree
  in process rather than scraping `--help`, so hidden commands (`serve`, `lsp`)
  do not register as drift, and it reads only fenced blocks and code spans, so
  prose such as "reach for prowl before grepping" is not mistaken for a
  command.

### Changed
- The index schema is bumped twice (v3 to v4, then v4 to v5) to store the per-file
  redaction count and the masked doc-comment field. Opening an older index with
  this release migrates it in place, backing up the old database first; the new
  fields populate on the full reindex that upgrading to a new binary already
  triggers, so an existing user's index is rebuilt once on the first command
  after the upgrade.
- `status` and `overview` no longer lump external module imports with broken
  references under one scary "dangling" count. Edges are now reported as
  resolved (point to a repo file), external (module/package deps, expected), and
  unresolved (genuine gaps) -- the last is the number to watch.
- MCP reranking prefers the local model (a direct LLM integration, the MCP
  2026-07-28 replacement for deprecated sampling) and falls back to host
  sampling only when no local model is configured.
- `init` prints a compact, bordered "ready" card (indexed counts, language mix,
  integrations, next steps) on a terminal instead of a wall of per-skill-file
  plan lines; the `--dry-run` plan collapses the per-agent skills into one line.
  Piped/`--json` output is unchanged.
- `init --integrations auto` (the default) is more aggressive about making every
  agent know Prowl. It always registers Prowl's MCP server via a client-agnostic
  `.mcp.json` and writes the `AGENTS.md` guidance; additionally it now detects the
  harnesses you actually run (omp, Claude -- by their config dir or launcher, not
  only a project config dir) and installs their native integration too: the prowl
  skills that tell an agent when to reach for Prowl, plus omp's native
  `.omp/mcp.json`. Prose alone is unreliable for coding agents with strong native
  grep/read priors (notably Anthropic models), which under-weight an instruction
  to shell out to an unfamiliar CLI; a registered MCP server puts Prowl's tools in
  the agent's own tool list, where they actually get called. The `AGENTS.md` block
  and the exploration skill now teach both interfaces -- the MCP tools (preferred)
  and the `prowl` CLI (the opt-in equivalent) -- and the interactive picker
  pre-selects this whole baseline. For omp it also installs a sticky
  `.omp/RULES.md` (re-injected near every turn, unlike `AGENTS.md`, which drifts
  up a long transcript) holding the "reach for Prowl before grep" directive in
  force. All reversible with `--remove-integrations`.
- The two weaker shipped skill descriptions now state their boundaries, because a
  description is the only thing an agent reads when deciding whether to reach for
  prowl at all. `prowl-change-safety` says what it does not do (it reports what
  depends on the code you are touching; it does not run your tests, linters, or
  build). `prowl-durable-knowledge` triggered on a self-assessed outcome
  ("something worth remembering"), which asks the agent to judge importance before
  it has any criteria; it now triggers on a problem state -- having just resolved
  something that cost real effort and will recur -- and draws the line against
  transient task state, which is `wip`'s job. Shipped skills are also checked for
  their contract invariants: the frontmatter `name` must equal the directory name,
  since the install path is `<client>/skills/<dir>/SKILL.md` and a mismatch
  installs a skill under a name nothing resolves.

### Fixed
- Newly created model sets now appear immediately and cannot be erased by an
  older in-flight refresh. A populated set editor opens on that set's selected
  models, while an empty set opens the candidate catalogue immediately and
  stays in add mode across membership changes; `a` switches between add and
  selected-only views. The console keeps its bottom keybind dock visible while
  transient status appears in the page heading instead. Harness injection now
  advertises only canonical
  `auto`, which follows the active set and strategy selected in Prowl, instead
  of exporting every strategy and named-set override into client model pickers.
  Re-injection also removes both retired OMP provider identities so an older
  block cannot keep duplicate routes visible.
- The unified console no longer accumulates deleted test and temporary
  workspaces as projects; registry reads now retain only resolvable indexes and
  compact stale or duplicate entries atomically. Browser OAuth now outlives the
  HTTP request that starts it, listens on both IPv4 and IPv6 localhost, and asks
  OpenAI for the connector scopes Codex requires, restoring Claude and ChatGPT
  sign-in. Gateway diagnostics are routed to `gateway.log` before either console
  entry point paints, so a rejected refresh can no longer print raw upstream
  JSON over the TUI or leave duplicate tab rows. OAuth exchange errors are
  bounded, and the Hyper subscription is consistently branded Charm Hyper.
- Project rows now show measured token savings, and `Enter` opens a scrollable
  full status report. Claude OAuth requests an authorization code explicitly,
  successful connection state remains visible with its enrollment next step,
  and account details report Claude's five-hour and weekly windows. Charm Hyper
  reports its live credit balance and the published 100-credit monthly grant
  without inventing a daily limit or account-specific reset time the API does
  not expose. Named routing sets now support create, edit, rename, confirmed
  delete, reliable Escape navigation, and provider, access, and intelligence
  filters across free, paid, and enrolled subscription models. The provider
  directory now includes each production-wired platform exactly once, excludes
  unsupported inference wires, and gives every row working detail, signup, and
  credential actions.
- `overview` entrypoints name code that starts the program. The rule was "has
  outgoing dependency edges and nothing depends on it", which also admits every
  manifest, config table, and test harness: a `package.json` carries one edge per
  dependency and nothing imports a test, so both outranked the real entry points,
  and the list was sorted by path depth, which inverts on any project whose entry
  is nested. Across six varied repositories 68% of the entries shown were manifests
  or test files, and 100% on a docs-heavy one -- in the first answer an agent reads
  to orient itself. Only languages that can start a program now qualify (`json`,
  `yaml`, `toml`, and `markdown` are excluded; `ini`, `hyprlang`, and CSS stay
  eligible, because in a dotfiles or theming repo the file that sources every other
  config really is the entry point), test code never qualifies, and survivors rank
  by how much each pulls in. Most importantly, an import from an excluded file no
  longer disqualifies its target: a unit test importing `main` was hiding the
  entry point of every project whose main module has a test.
- `init` no longer fails outright when a destination is a symlink. A repository
  serving several agent harnesses commonly points `AGENTS.md` at `CLAUDE.md`, and
  prowl refuses to write through a symlink because that is how a write escapes the
  project root -- but it took the whole setup down with it, including the
  integrations it could write, and reported only `setup operation failed`. That
  string was every setup failure: `safeError` collapsed permission errors, missing
  paths, and symlinked destinations into one line with nothing to act on.
  Unwritable destinations are now identified when the plan is built and reported as
  skipped with a reason, the rest of the plan applies, and prowl's own
  project-relative reasons pass through while OS errors are reduced to their
  classification rather than erased. The symlink and its target are still never
  written.
- `scripts/onboarding-smoke.sh` passes again. Its plan assertion required the
  action set to equal `{agents, cursor}`, so it broke as soon as per-skill install
  actions joined a selected client's plan and had been failing since; no CI job ran
  it, so nothing caught it.
- A prowl upgrade now heals its own index before answering anything. The indexing
  logic version is the binary's revision, so a new binary already forced a full
  re-parse -- which re-chunks every file and thereby drops every vector with it.
  The embedding backlog was then refilled only two seconds per command, so
  semantic search silently sat near-empty for hundreds of invocations after an
  update. The backlog is now drained during that same refresh, so the first
  command after `prowl update` returns with whole semantic coverage. The
  per-invocation ration is gone entirely: it only ever existed to survive remote
  embedding at ~47 chunks/second, and the bundled in-process embedder does ~650.
  Measured on an 8.5k-file repo without deleting the index: first command after an
  upgrade takes 43s (full re-parse plus 80,379 chunks re-embedded) and narrates
  itself on stderr once it passes a two-second grace period; the next command is
  0.98s and silent. The embedder itself ships inside the binary, so `update` needs
  no separate model download.
- Vector search returned nothing but blank lines. The chunker emitted a chunk for
  every run of blank lines between symbols (982 of them on an 8.5k-file repo, 2814
  with three or fewer non-space characters). A static embedder maps a
  whitespace-only chunk to its degenerate mean vector, which sits nearer an
  arbitrary query than real code does, so those chunks monopolized k-nearest-
  neighbour results: a direct probe returned 10 blank chunks out of 10, and because
  reciprocal-rank fusion interleaves the vector and lexical lists, **half of every
  top-50 result page was empty** (25/50 measured). Content-free chunks are no
  longer emitted, are never embedded, and vectors stored for them by an earlier
  version are pruned on the next pass. Blank results in the top 50: 25 -> 0, and
  the vector half now contributes 4-7 of the top 10 results that lexical search
  alone never surfaced (for "keep secrets out of the log file" it adds
  `agent/secret_scope.py`).
- Embeddings always come from the code embedder bundled in the binary
  (`potion-code-16M`), never from Ollama. Preferring an Ollama embed model when one
  happened to be installed cost 14x throughput (47 vs ~650 chunks/second measured),
  which is why a first semantic index on an 8.5k-file repo took ~30 minutes instead
  of 44 seconds. Worse, stored vectors are keyed by their producing model, so
  choosing the embedder by "is the daemon up right now" silently invalidated and
  rebuilt the entire vector index whenever Ollama or a model came or went. Ollama
  and coding-agent CLIs now serve only the generate/rerank half, which genuinely
  needs an LLM.
- AI tiers no longer name an embedding model, because they never controlled
  embeddings. `ai.embed_model` is gone from project and global config, `--tier`
  selects only the optional rewrite/rerank model, and `init` no longer pulls or
  warms an embed model or claims "semantic search ready" after embedding nothing
  (it opened the project with AI disabled, so it never embedded at all).
- The `--smart` query rewrite described the wrong corpus. Its prompt asked for
  "a short keyword search query for a dotfiles/config index" -- a leftover from
  prowl's origins -- steering rewrites of code questions toward configuration
  vocabulary. It now asks for identifiers, type and function names, and domain
  nouns. On "keep secrets out of the log file" the rewrite yields
  `secret credential token key log logger logfile redact sanitize mask filter` and
  surfaces `agent/redact.py`, which neither half found on its own.
- Large repositories work again. `overview` (and `explore`, `init`'s AGENTS.md
  map refresh, and the `overview` MCP tool/resource) hard-failed with
  `overview files exceeds limit 10000` on any repo above 10k files, plus matching
  caps at 50k dependency edges, 10k resource links, and 10k keybinds. Overview is
  an agent's first call, so refusing it left large repos with no map at all --
  while `clusters`, which does the same connected-component work with no cap,
  succeeded on the same repo. The repo-size caps are gone; only Overview's output
  shape stays bounded (8 docs, 20 entrypoints, 8 subsystems, 16 palette entries,
  5 hotspots). Verified on a 15,568-file / 793k-symbol checkout: 2.5s warm.
- Overview counts are exact. They came from sentinel-capped SQL
  (`count(*) FROM (SELECT 1 FROM t LIMIT ?)`), so above the cap the reported file
  and language totals were the cap itself, and the language histogram covered
  only a bounded prefix of the file table. `overview` and `status` now report the
  same numbers, from one shared exact-counting path.
- The first semantic index no longer restarts from zero. Embedding ran inside the
  atomic index generation, so every interruption rolled back the whole pass; on a
  repo needing ~30 minutes of model round trips (83k chunks) the vector index
  could never finish. Vectors are a derived cache and are now built against the
  published store, committing each batch, so an interrupted build resumes from
  the remaining backlog.
- An incomplete embedding backlog no longer marks the whole index stale. Any
  command with AI enabled re-indexed the entire repository and then blocked
  indefinitely on the backlog with no output, which is what made `search` appear
  to hang on a large repo. Implicit refreshes now do a time-bounded embedding
  pass; `init` drains the backlog explicitly and reports progress.
- Semantic search is no longer switched off repo-wide while the backlog drains.
  It required a fully drained backlog, which on a large repo is almost never
  true. Partial vector coverage is now used, with lexical ranking covering the
  rest, and `status` reports coverage (`Semantic: building -- N of M chunks
  embedded`) so a partially built index is visible instead of looking like a
  hang.
- `init` builds the semantic index it advertises. It opened the project with AI
  disabled, so it printed "semantic search ready" having embedded nothing.
- `search` (and the `similar_code` / `smart_search` MCP tools) now surface files
  whose path names the concept. It ranked purely by full-text over chunk bodies,
  so a file literally named `stash-download.sh` never surfaced for "download" if
  another file merely mentioned the word more often, and an exact-token search
  for "downloader" missed the file's "download" entirely. Search now injects
  files whose path matches the query terms (lightly stemmed, so "downloader"
  finds `download`) and floats path-named matches to the top of their tier, so
  the file that implements what you searched for leads instead of tangential
  mentions.
- `init` no longer reports "0 symbols, 0 edges" on a re-init that changed nothing.
  The summary showed the incremental delta instead of the index totals; it now
  reports the real indexed files, symbols, and edges.
- `graph` now maps the whole repo instead of a truncated subset. It drew only
  files that had resolved dependency edges and hard-capped at 800 nodes, then
  labeled that subset "N files" -- so a 2,237-file repo showed "800 files" and
  looked like most of the code was missing. Every indexed file is now a node
  (standalone files as small dots, hubs as large ones), the draw ceiling is
  3,000, and when a repo exceeds it the header honestly reads "N of M files".
- The `status` card no longer wraps text mid-word. Language names (`javascript`,
  `markdown`) and long project names were rendered into a column narrower than
  the text, so they wrapped and knocked every bar out of alignment. The card was
  rebuilt on a fixed content width where each column truncates instead of wraps,
  so labels, proportional bars, and counts line up. It also gains accent section
  headers, a token-savings hero, home-relative paths, and a status dot.

### Added
- `init` now indexes a project's real stack even when `.prowl/config.toml` carries
  a stale `languages` filter copied from another project. When the filter would
  leave more of the repo's code unindexed than indexed, init resets it to `auto`
  (with a notice) so the dominant language is not silently excluded; a filter that
  keeps the majority of the code is respected. `init` and `status` still warn
  about any language that is present on disk but excluded from the index.
- `init --languages <list|auto>` sets the indexed-language filter in one command,
  so the warning above has a one-line fix (`init --languages auto`) and indexing
  can be scoped at setup without hand-editing `.prowl/config.toml`.

- External documentation can be ingested and searched. `prowl docs add <url>`
  crawls a documentation site (bounded depth, page cap, rate limit, robots.txt) and
  converts each page to clean Markdown; `docs add <dir> --local` ingests a local
  Markdown tree. Pages are stored in a shared per-machine corpus and indexed, then
  `docs search <question>` (and the `search_docs` MCP tool) return a small, cited,
  budget-bounded context packet with the same engine used for code, so an agent
  queries library docs without pulling whole pages into context. Retrieval needs no
  model. Crawled content is untrusted, so pages containing prompt-injection
  directives are quarantined out of the searchable corpus. `docs list` and
  `docs remove` manage sources. When a site publishes an `llms.txt` / `llms-full.txt`
  (the agent-friendly documentation convention), `docs add` uses it directly: one
  fetch of the whole docs as Markdown, or the curated page list, instead of
  crawling navigation. `--no-llms` forces a plain crawl.

- Knowledge can be captured from structured fields instead of hand-authored OKF.
  `knowledge propose` takes `--type`, `--title`, `--body`/`--body-file`,
  `--resource`, `--tag`, and `--anchor path#symbol` (or `path:start-end`), and the
  `propose_knowledge_change` MCP tool takes the same fields; prowl assembles a
  valid document and runs it through the normal review, diff, and anchor-hashing
  path. This removes the need to know the OKF frontmatter layout to record a
  decision or claim.
- A candidate that places a prowl field (`anchors`, `status`, `confidence`,
  `related`, `valid_from`, `valid_to`) at the top level instead of under `prowl:`
  is now rejected with a clear error. It was previously parsed and the misplaced
  field silently ignored, so an author could believe an anchor was recorded when
  it was not.

- Knowledge source anchors can pin to a `symbol` (function, class, or component
  name) instead of `line_start`/`line_end`. The symbol's range is re-resolved
  from the index on each `knowledge propose` and `lint`, so the anchor follows
  the symbol when lines move above it and goes stale only when the symbol's body
  changes. Line-range anchors previously went stale on any insertion above the
  region. The `Anchor.Symbol` field existed in the model but was not resolved; it
  is now used by `CheckAnchorResolved` and the CLI and MCP paths.

- An `agent-skills` setup integration installs prowl's skills into `.agents/skills/`,
  the harness-agnostic Agent Skills standard location (agentskills.io) that Prime
  Agent and other standard-compliant harnesses discover -- not just the per-client
  dirs (`.claude/skills`, `.cursor/skills`, `.opencode/skills`) prowl already
  wrote. It is detected when a repo has `.agents/`, selectable via
  `--integrations agent-skills`, and reversible via `--remove-integrations`. This
  makes prowl's repo-exploration, change-safety, and durable-knowledge skills
  auto-discoverable by REPL/programmatic-tool-calling harnesses, which otherwise
  ship no code-intelligence and fall back to grepping and reading whole files.

- `context`/`search_context` now matches queries against file paths, not just
  symbols and chunk text. Developers name files after their purpose, so the path
  is deterministic concept context (the code-native form of contextual
  retrieval): a query like "project persistence" now finds `projectPersistence.ts`
  even when the file's body never repeats its own name. A file whose basename
  carries the query's concept terms is recalled and ranked up; the gate requires
  two distinct terms for multi-word queries so incidental single-term hits don't
  flood results. Found by dogfooding a TS repo where `projectPersistence.ts` was a
  total recall miss; the retrieval benchmark is unchanged (recall 1.0).

- `doctor` now flags a language that is present on disk but unindexed -- almost
  always a `languages` filter in `.prowl/config.toml` that omits the repo's real
  stack (e.g. a Go project carrying a config copied from a QML/rice setup), which
  otherwise leaves prowl with a near-empty index and *no* symptom except empty
  query results. It compares the ignore-aware file walk against the indexed
  languages and warns, e.g. "237 go files present but 0 indexed -- the
  `languages` filter likely omits \"go\"". Found by dogfooding on a real Go repo
  whose stale config indexed 0 of its 238 Go files while `doctor` reported a
  perfect 100/100.

- `prowl outline <path>` and the `outline` MCP tool return a file's
  structure -- every symbol with its kind, signature, nesting depth, and line
  range, but no bodies -- from the index alone (no file read). An agent grasps a
  file's shape from a handful of signature lines instead of reading the whole
  file (66% fewer tokens on a 332-line QML service in testing), then reads only
  the symbols it needs with `def`/`read_symbol`. `outline` joins the core MCP
  surface (now eight tools), directly targeting agent token usage.

- The `find_references` MCP tool exposes symbol-level call-hierarchy on the core
  surface (now nine tools): an agent gets the cited `{file, line, text}` call
  sites of a function or symbol -- its callers and change blast radius -- in one
  call instead of grepping and reading files. `references` (CLI and tool) now
  resolves a symbol by name as well as by id (like `def`), so no find-then-id
  round trip, and returns a helpful "run find" error for an unknown name.

### Changed

- `knowledge propose` (and the `propose_knowledge_change` tool) now compute a
  source anchor's `content_hash` from the current file when the author supplies
  only a `path` and line range. Authoring durable knowledge no longer requires
  hashing the region by hand -- the friction that previously left anchors with no
  hash, which lint then reported as permanently `stale_anchor` ("changed") even
  though the code never changed. Anchors that already carry a hash are untouched;
  an unreadable path or out-of-range span is left for lint. Rewritten anchors now
  also serialize `line_start`/`line_end` as YAML integers, not quoted strings.

- Go `_test.go` files are no longer treated as part of a package's imported
  surface. The cross-package `pkg` fan-out was linking every external importer to
  every file of the imported package, including its test files -- so a test file
  inherited the package's whole in-degree. `impact` on a test file falsely
  reported dozens of dependents (e.g. `service_test.go` showed 23), and test
  files surfaced among `brief`'s "key files to read first" and inflated
  `hotspots`. Now importers link only to the package's non-test files: a test
  file's blast radius is correctly empty, and `brief` leads with implementation.

- Package/namespace imports that resolve to an in-repo target (Go packages, C#
  and Elixir namespaces) are now marked resolved instead of being left as
  unresolved import edges. They fan out to per-file `pkg` edges for
  callers/impact, but the originating import edge stayed `resolved:false` -- so
  `callees` showed a file's own internal dependencies as unresolved (identical to
  external stdlib), and the dangling-edge count was inflated. On Prowl's Go
  tree this cut false danglings by 251 (1,334 -> 1,083) and `callees` now marks
  internal imports `resolved:true`; `callers`/impact counts are unchanged (the
  import is resolved to a non-file target, so it is not double-counted).

- `--format json` now renders an empty result as `[]` instead of `null` (and
  TOON as an empty collection). An agent consuming prowl programmatically -- the
  norm in REPL / programmatic-tool-calling harnesses -- can iterate a `find`,
  `callers`, or `search` result directly without a nil guard; `null` previously
  raised "not iterable" on any empty result. Populated results are unchanged.

- Indexing now skips generated dependency lockfiles (`package-lock.json`,
  `go.sum`, `Cargo.lock`, `*.lock`, ...) and minified bundles (`*.min.js`,
  `*.map`) entirely. These carry no code-intelligence value but expand into
  thousands of meaningless "setting" symbols -- a single `package-lock.json`
  emitted 9,022 symbols on a real TS repo, 90% of its whole index -- which bloat
  storage and pollute `find`. Their real information (declared dependencies)
  lives in the manifest, which is still indexed. On that repo the symbol count
  dropped from 18,280 to 9,258. Found by dogfooding a TypeScript/Electron app.

- `context`/`search_context` now treats test files as a low-signal class,
  down-weighting them for "how does X work" questions so the implementation that
  defines the behavior ranks above the tests that exercise it. Tests match the
  same identifiers densely and otherwise outrank the real code; a query that
  names testing (e.g. "how is X tested") exempts them. Detection is by delimited
  path token (`tests/`, `dev-testing/`, `test_x.py`, `x_test.go`, `x.spec.ts`),
  so "attestation"/"latest" are unaffected. Found by dogfooding a Python bot
  where the test harness outranked the real client; the retrieval benchmark is
  unchanged (recall 1.0).

- `context`/`search_context` ranking now matches inflected query words to
  base-form symbol names when awarding the symbol-authority boost: "how are
  files parsed in parallel during indexing" now recognizes that `index.go`
  defines `parseFile`/`indexWithOptions` (via light stemming -- "parsed"->"pars",
  "indexing"->"index", "files"->"file"), so the file that actually defines the
  queried concept ranks first instead of a file with only a coincidental plural
  match. Found by dogfooding on a Go codebase; the retrieval benchmark is
  unchanged (recall 1.0).

- Indexing writes each file's symbols, resources, edges, and chunks with
  batched multi-row INSERTs instead of one statement per row, collapsing the
  per-row CGO and prepare round-trips that had become the dominant cold-index
  cost. On a 1,413-file repo the cold index dropped from 6.7s to 2.9s (with the
  earlier parallel-parse and query-cache work, 13.8s to 2.9s overall -- ~4.75x).
  The published index is byte-identical (same symbols, edges, chunks, and
  resolution); incremental reindex is unaffected.

- Indexing compiles each language's tree-sitter query once per process and
  caches the compiled grammar, instead of recompiling the query on every file.
  Query compilation was ~42% of indexing CPU (it ran once per file); caching it
  roughly halves total indexing CPU. On a 16-core machine the cold-index wall
  time improves modestly (the serial store writer is now the bound), but the
  saving is larger on fewer cores and leaves more headroom when the MCP server
  reindexes while serving queries. The index is byte-identical to before.

- Indexing now parses files in parallel across CPU cores instead of one at a
  time. Reading and tree-sitter extraction (the CPU-bound work) fan out to a
  worker per core while the store stays single-writer; results are consumed in
  traversal order, so file IDs and the published index are byte-identical to the
  serial build (deterministic across runs). A cold index of a 1,413-file repo
  dropped from 13.8s to 7.1s (~1.9x) on a 16-core machine. Incremental reindex
  (only changed files) is unaffected.

- The AGENTS.md guidance Prowl injects on `init` now teaches `references <name>`
  (a symbol's callers) instead of the stale `references <symbol_id> (id from
  find)` -- the tool resolves by name directly now, so agents skip the
  find-then-id round trip.

- `find` now tolerates typos: when a name matches nothing exactly, by full text,
  or by substring, it falls back to symbols within a small edit distance
  (optimal string alignment, so a wrong letter, an adjacent transposition, or a
  missing letter still resolves -- `BatteryIndecator` finds `BatteryIndicator`).
  It fires only on a total miss so it never displaces a real match, skips very
  short queries to avoid noise, and stays fast (a bounded, length-filtered scan).

- Path arguments are normalized before lookup, so a leading `./` or an uncleaned
  path (`pkg/../pkg/file.go`) resolves like the clean form. Agents routinely pass
  `./path`; previously `outline`, `impact`, `callers`, `callees`, `relations`,
  `entrypoints`, and `tests` returned a false "not indexed" or an empty result
  for it, wasting a tool call. Now they resolve.

- The AGENTS.md guidance Prowl injects on `init` now points agents at `outline`
  (a file's structure without reading it) and `def` (one symbol instead of the
  whole file), and frames the workflow as reading through Prowl rather than
  opening whole files. Agents only save tokens if the guidance names these
  commands; the block previously listed neither, so the token-saving read path
  went unused.

- Repository-meta and CI configuration (`.github/`, `.gitlab-ci.yml`, CircleCI,
  Jenkinsfile) is now a low-signal class, so an issue template or workflow stops
  outranking real code for a code question; a query that names CI (workflow,
  release, pipeline) still surfaces it. The symbol-authority boost also no longer
  lifts a low-signal file whose key coincidentally matches the query, so a locale
  or generated table can't ride the boost back over code.

- Context ranking no longer counts stopwords ("how", "the", "is") in lexical
  scoring, so verbose prose and generated data files stop outranking the actual
  code for a natural-language question. A candidate that *defines* a symbol
  matching the query (a file with `ComputeFrame` for "how is a frame computed")
  is also boosted above one that only mentions the terms in text, since naming a
  symbol after a concept is a deterministic authority signal. A new retrieval
  benchmark case (authoritative source vs term-dense prose under a tight budget)
  guards both; on inir the battery service rose from rank 7 to 4 and an
  unrelated term-dense widget fell out of the packet.

- QML `function` and `signal` declarations are now indexed as symbols, so
  `find`, `def`, and `read_symbol` resolve a component's methods and signals
  (previously only components, properties, and ids were indexed). In a
  QML-heavy project this makes component methods first-class and readable like
  functions in any other language.

- `def` and the `read_symbol` MCP tool now include a symbol's doc comment: the
  contiguous block of comment lines directly above it, stopping at the first
  blank line and bounded to 15 lines so a file-top license header is never
  absorbed. Reading one symbol now carries its documentation, matching what an
  editor's hover shows.

- Full-text and embedding chunks are now aligned to symbol boundaries instead of
  blind 40-line windows, so a function, type, or component stays whole in one
  chunk (up to twice the window) rather than being split across two. A symbol
  split mid-body fragments its retrieval signal; keeping it whole is the proven
  structure-aware chunking win (cAST, EMNLP 2025). On a 1,400-file repo the
  share of multi-line symbols contained in a single chunk rose from 79% to 98%.
  Files with no extracted symbols still chunk into fixed windows.

- Content search now has a substring fallback: when the full-text tiers leave
  slots unfilled, a query for a word recalls chunks that only contain it inside
  a camelCase compound (e.g. "battery" in "batteryChargeLevel"), which the FTS
  tokenizer keeps whole. It runs only after the ranked FTS tiers, so precise
  matches still lead. This closes a recall gap that previously left content
  search unable to find component words in compound identifiers.

- `init` now configures MCP clients with the lean `core` tool surface
  (`serve --mcp-surface core`), the curated six-tool set with progressive
  disclosure. The injected config previously launched bare `serve`, which
  defaults to the eighteen-tool legacy surface, so clients never actually saw
  `search_context`/`get_context` or their improved descriptions and ranking.
  New and re-initialized clients now get the intended surface; the bare `serve`
  default is unchanged for manual and opt-in use (`--mcp-surface legacy|all`).
- Full-text search merges its phrase, all-terms (AND), and any-term (OR) tiers
  instead of returning only the first non-empty tier. Precision matches still
  lead, but the OR tier fills the remaining slots, so a natural-language query
  recalls files that share only some of its terms instead of being masked when a
  coincidental file happens to match every term. On inir, `search "battery
  status display"` now surfaces the real `PillBattery` and `BatteryPopup`
  components the all-terms tier alone missed. This lifts recall for both `search`
  and the MCP context packets, which share the retrieval, and ranking (low-signal
  down-weight, graph, semantic) keeps precision.
- Context ranking now also demotes prose documentation (Markdown, README,
  CHANGELOG) below code for a code question. The tier merge above surfaced more
  doc chunks, so a "how does X work" query started leading with `docs/*.md` and
  `CHANGELOG.md` instead of the code that answers it. Docs are dampened mildly
  (a doc can be the answer), and a query that names docs or guides keeps them at
  full weight. This completes the low-signal ranking work for the doc case the
  original backlog flagged.
- Context ranking now down-weights low-signal file classes (locale/i18n tables,
  generated code, dependency lockfiles, minified bundles) so a keyword-dense
  data file no longer outranks the real code that answers a question. The
  dampening is query-aware: a query that names the class (for example
  "translation" or "lockfile") still surfaces those files, and a direct
  identifier match is never dampened. A seventh, provider-free benchmark case
  reproduces the failure and guards the fix.
- QML dependency resolution now models singleton and type member references
  (`Config.spacing`, `Theme.accent`), not just component instantiation. A shared
  singleton is used by member access and never instantiated, so `impact` and
  `callers` previously reported zero dependents for it. On a real 1,400-file
  Quickshell repo, `impact` on the `Config.qml` singleton went from 0 to 978
  dependents and resolved edges rose from 3,681 to 7,327, with no new dangling
  edges (unresolved built-in references like `Qt` and `Math` are dropped).
- `callers`, `callees`, and entrypoint-root detection now traverse QML component
  instantiation and singleton member use, not just imports. `callers` on a shared
  `Config.qml` went from empty to its 413 users on the same repo, and components
  that are instantiated elsewhere are no longer miscounted as entrypoints.
- The six MCP core tools now carry explicit when-to-use and when-not-to-use
  descriptions, cross-reference each other (`search_context` then `get_context`),
  and state the freshness guarantee (Prowl reindexes changed files before
  answering), so an agent reaches for the index instead of grepping or reading
  whole files. Precise tool descriptions measurably improve how an agent picks
  the right tool.

- Refocused Prowl into a single, agent-first token-saving context engine
  delivered over the CLI and MCP. Removed the browser workbench (`prowl
  open`, the embedded web UI, and the HTTP workbench server), the durable-jobs
  and event-streaming plane, and the Hermes-inspired agent-operations kernel
  (operations, agent, session, profile, toolruntime, entity) with its `session`
  command. The retained engine (index, query, context packets, knowledge,
  capabilities, MCP, and LSP) is the whole product.
- Overview/brief projections no longer refuse repositories above 100k symbols;
  the display-only count ceilings were raised, and a missing index on the
  self-referential `symbols.parent_id` cascade that made full re-indexing of
  large projects run in O(n^2) was added, cutting a multi-minute stall to
  seconds.
- The generated `AGENTS.md` guidance is now a directive rule rather than a menu:
  query Prowl to find code, trace how it connects, or check a change's blast
  radius, and fall back to grep or file reads only for plain-text scans. It
  states that queries reindex first (so answers are current) and lists `wip`.
- `init --integrations auto` (the default) now always writes the `AGENTS.md`
  guidance, even on a repo that has none, so a bare `init` still tells agents to
  reach for Prowl. It stays a reversible, marker-bounded block.
- `init` no longer leaves untracked runtime files for `git add -A` to sweep up:
  `.prowl-setup.lock` and `.prowl/setup-applies.json` are now covered by the
  derived-state gitignore block alongside the index, cache, logs, and editor
  state.
- `search_context` now tells the agent, in its tool description and the `rerank`
  hint, that reranking uses the agent's own model and needs no local model, so
  the agent knows when to ask for it. When the client does not support sampling,
  ranking stays deterministic and no query is blocked to prompt a download.
- Fixed the release build so it publishes across every platform, and releases
  are now cut on demand by running the build-binary workflow with a `version`
  input (no personal access token or hand-pushed tag needed). The `darwin-amd64`
  target, which never scheduled on the retired Intel macOS runner and stalled
  every release for 24 hours, is cross-built on the arm64 runner (build-only,
  since the macOS SDK is universal). Windows now self-compiles go-sqlite3 with
  the runner's mingw gcc and takes only the sqlite3 header from vcpkg, fixing an
  undefined-symbol link failure. The full test suite no longer runs in the
  release build (CI already runs it on Linux), the Windows `.exe` is no longer
  stripped (stripping trips more antivirus false positives), and every build job
  has a timeout so a stuck runner fails in minutes, not a day.

### Added
- `prowl def <name-or-id>` and the MCP `read_symbol` tool return one
  symbol's source (signature and body), cited and bounded, resolved by name or
  by a find id. An agent reads a single function, type, or component instead of
  the whole file, which is prowl's core promise applied to the find-then-read
  step. A QML component returns its whole file; other symbols return their exact
  range, capped at 200 lines. `read_symbol` joins the core MCP surface, so
  MCP-only clients get symbol-level reads too.
- `prowl brief <path>` gives a cited orientation for a path or subsystem
  in one call: file count, languages, the architecture guides to read, and the
  key files to read first (ranked by graph centrality). Use it to warm-start on
  a slice of the repo, or to hand a subagent scoped context, instead of
  re-deriving the shape by reading files.
- `hotspots` and the `overview` first-call map now rank files by graph centrality
  (a PageRank over the resolved dependency graph); `hotspots` exposes it as a
  `central` field beside the existing `fan_in`.
  Centrality captures the QML/desktop coupling that raw in-degree omits
  (component instantiation and singleton member use), so architectural hubs
  surface first: on a 1,400-file Quickshell repo the top central files are the
  `Appearance`, `Config`, and `ColorUtils` singletons.
- MCP tool results now reach the model as TOON instead of JSON. The read tools
  serialized their output as JSON in the content block the model reads; TOON
  carries the same data for roughly 40% fewer tokens, and the structured output
  still travels as JSON in `structuredContent` for clients that parse it. This
  brings the MCP surface in line with the token-lean CLI default.
- Agent skills: `init` installs a small set of prowl skills (repo exploration,
  change safety, durable knowledge) into a detected harness skill directory
  (`.omp/skills/`, `.claude/skills/`), so agents learn when to reach for prowl
  instead of grepping. Each skill's description states its triggers. The
  installed skill files are excluded from the index. AGENTS.md remains the
  fallback for harnesses without a skill system.
- `explore <path>` reviews or extracts from a repository you do not own: it
  indexes into a scratch location, answers an overview or a `--question` context
  packet, and leaves the target tree untouched (no `.prowl/`, `.gitignore`, or
  `AGENTS.md` written into it).
- Host-powered reranking: `search_context` accepts `rerank`, and when the MCP
  client supports sampling, prowl asks the host model to reorder results. This
  gives semantic-quality relevance with no local model, so a keyword-dense file
  no longer outranks the real code. Falls back to deterministic ranking when
  sampling is unavailable.

- `context search`, `context get`, and `context traces` expose versioned,
  budget-aware packets that combine canonical project knowledge with cited source
  and one-hop dependency evidence. Retrieval stays deterministic without a model,
  while optional semantic reranking can refine results without changing fallback
  behavior.
- MCP now offers additive Resources and Prompts on every compatibility surface,
  plus a six-tool `core` surface for context, change analysis, review-only
  knowledge proposals, validation, and capability discovery. Sampling and human
  elicitation remain optional; proposal writes fail closed when host confirmation
  is unavailable.
- `capabilities search` and `capabilities get` provide token-lean discovery of
  built-in Prowl workflows before clients load detailed tools or resources.
- `wip` (CLI) and `investigate_wip` (MCP) report uncommitted work in progress:
  the files touched since the last commit with their git status, the
  unfinished-work markers inside them (TODO, FIXME, HACK, XXX, BUG, WIP,
  OPTIMIZE, or a custom `--markers` list), and the blast radius of each indexed
  file. It lets a fresh session resume without re-reading the tree.
- Continuous integration (`.github/workflows/ci.yml`) runs gofmt, vet, build,
  and the tagged test suite on every push and pull request. A version workflow
  advances the patch on each push to `unstable`, rolling the tenth patch into
  the minor. The pre-commit hook now also blocks Go files that are not
  gofmt-clean.

### Changed

- `tests <path>` now finds the test files covering a source file -- the test
  files in its own directory (the common layout where tests sit beside source),
  or, failing that, the test files that import it (a Java/C# `src/test` mirror or
  an external test package) -- with the conventional match (`os.go` ->
  `os_test.go`) listed first. It previously only reported configs/keybinds that
  launch a file, so on a code repo `tests os.go` wrongly said "no tests detected"
  while `os_test.go` sat right beside it. The launcher behavior remains as the
  fallback for a config or script with no tests.

## [0.8.1] - 2026-06-21

### Fixed

- `references <id>` for a function or method now matches call syntax (the name
  invoked with `(`), not every bare mention, so a signature-change blast radius
  excludes config/struct fields and identifiers that merely share the name. On
  lazygit, `references` for the `CopyToClipboard` method went from 40 noisy hits
  (22 of them non-calls that also crowded real calls out of the result cap) to
  the exact 23 call sites across 8 files -- matching a careful hand-grep.

### Changed

- `references <id>` for a function or method now matches call syntax (the name
  invoked with `(`), not every bare mention, so a signature-change blast radius
  excludes config/struct fields and identifiers that merely share the name. On
  lazygit, `references` for the `CopyToClipboard` method went from 40 noisy hits
  (22 of them non-calls that also crowded real calls out of the result cap) to
  the exact 23 call sites across 8 files -- matching a careful hand-grep.
- `search <text>` ranks a project's implementation above tests, then docs and
  i18n/locale string tables, then vendored/generated code. Lexical search
  otherwise floats the files that carry human-readable feature wording (string
  tables, translated docs, integration tests) above the code that implements it:
  a concept query like "stage or unstage a line or hunk" on lazygit buried the
  real patch-handling source at ranks 9-14 under i18n/docs/tests; it now leads.
  Demotion only -- nothing is dropped.

## [0.8.0] - 2026-06-21

### Added

- Elixir is a first-class language. A Tree-sitter extractor records modules
  (`defmodule`), functions (`def`/`defp`/`defmacro`), and `alias`/`import`/`use`
  edges; each module name is recorded like a namespace, so an `alias MyApp.Foo`
  resolves to the file declaring `defmodule MyApp.Foo` (the C# namespace model,
  generalized). Validated on Phoenix: 297 module dependencies resolved across
  287 files, so `impact`, `callers`, `clusters`, and `references` work on an
  Elixir/Phoenix project. The `alias A.{B, C}` group form is expanded.

### Changed

- `find <name>` ranks its results so a project's own code definitions come
  before config/doc entries (settings, headings) and before vendored or
  generated files, keeping the match-quality order within each tier. On a
  config-heavy repo a query like `find run` previously buried the actual `run`
  methods under dozens of YAML `run:` keys; the definitions now lead. No results
  are dropped, so config-only lookups are unaffected.
- `references <id>` tags each code call site with its enclosing function or type
  (an `in` column), so the answer reads as which functions call a symbol
  (`NewGui` is called from `NewApp`, `initGocui`, `RunTUI`, ...) instead of a
  bare file:line list. Computed from indexed line ranges; the column is empty
  for usages at file scope (comments, top-level code). Lines that are themselves
  a same-named definition (an interface method, an override in another class)
  are dropped, so an overridden method's results show real calls, not siblings.
- `search <text>` demotes vendored and generated files so a project's own code
  leads. A dense dependency file (a generated constants table, say) used to
  monopolize the results by raw FTS rank -- `search status` on a Go repo
  returned 50 hits all from a vendored Windows-errors file and none of the
  project's status code; the query now pulls from a larger pool and floats
  project chunks first, keeping FTS order within each tier.
- `search <text>` falls back from an exact-phrase match to all-terms (AND) and
  then any-term (OR) only when the phrase matches nothing, so a non-AI,
  natural-language query returns the most relevant chunks instead of an empty
  result. `search "render the status panel"` (no verbatim phrase) now surfaces
  the files that draw and render the panel; an exact phrase still matches first.

## [0.7.0] - 2026-06-21

Language breadth, symbol signatures, and token-economy hardening on large
real-world repositories, all on top of the 0.6.0 index.

### Added

- PHP, Kotlin, and Dart are first-class languages now. Each has a Tree-sitter
  extractor (classes/interfaces/traits/enums/objects/mixins/extensions,
  functions and methods with cyclomatic complexity) and import-graph resolution:
  PHP `use Ns\Class` resolves to the file declaring that class (matched on the
  recorded namespace plus basename); Kotlin shares a JVM resolver with Java so
  the two resolve to each other across Maven/Gradle/Kotlin-Multiplatform source
  roots, folding a member or nested-type import to its enclosing class file;
  Dart resolves `package:<name>/...` to a workspace package's `lib/` (from each
  pubspec.yaml) and relative/part imports as paths. Validated on Laravel (8237
  `use` imports), OkHttp (1944, cross-language), and LocalSend (977 `package:`).
- Symbol signatures: every code language records a function/method/type's
  declaration header (name, parameters, return type, a class's extends/
  implements), so `find` and `search` hand the agent a symbol's interface inline
  instead of only a location. Signatures are full-text indexed too.
- TypeScript/JavaScript tsconfig path aliases: an import matching a `paths`
  wildcard (`@/components/Button` with `"paths": {"@/*": ["src/*"]}`) resolves to
  the real source, scoped to the nearest tsconfig and following a local
  `extends` to a shared base config. Validated on shadcn/ui (8382 `@/` imports).

### Changed

- `references <id>` returns precise `{file, line, text}` call sites for code
  symbols (the exact usage line, matched on an identifier boundary) instead of
  40-line full-text chunks.
- `overview`, `impact`, and `entrypoints <file>` cap large lists by default: an
  exact count plus a shallow-first sample, so a hub file or large codebase keeps
  the first-call answer token-lean (Laravel overview ~59 KB to ~2.5 KB, impact
  on a 1527-dependent file ~12 KB to ~1.2 KB). `impact --all` still lists every
  dependent.
- The generated `AGENTS.md` guidance steers agents to read a symbol's signature
  from `find` and cited call sites from `references` before opening files.

### Fixed

- Nested `.gitignore` files are honored, each scoped to its own directory
  (deeper rules win), so a monorepo's per-package `dist/`/`build/` ignores keep
  generated output out of the index.
- PHP/Kotlin/Dart external imports (`package:flutter/...`, `Symfony\...`) are no
  longer flagged as `dangling_includes` violations (Laravel `violations` ~106 KB
  to ~4.6 KB).
- A Dart `part of` directive no longer emits a reverse edge that faked a cycle
  with a generated companion file (`.g.dart`, `.freezed.dart`).
- The `AGENTS.md` block is refreshed in place by re-init without touching the
  user's own content, even when the closing marker is missing.

## [0.6.0] - 2026-06-21

The first working version: a local index that gives AI coding agents fast, cited
answers about a project's files, served over MCP.

### Added

#### Indexing

- Incremental indexing. An ignore-aware walk hashes files and reparses only what
  changed; deleted files are pruned, and graph resolution re-runs each time.
- The ignore-aware walk honors `.gitignore` negation and directory-only
  patterns: patterns apply in order (a later match wins) so a `!` line
  re-includes a subtree, and a trailing `/` restricts a pattern to directories.
  This is what lets a JS/TS monorepo that ignores a tree but keeps its source
  (`packages/*/*/` then `!packages/*/src/`) be indexed at all -- previously the
  whole `packages/*/src` source tree, and same-depth manifests like
  `packages/<pkg>/package.json`, were skipped (on tRPC: 809 -> 1470 files).
- A binary upgrade forces a full re-parse: the index records the binary's version,
  so extractor and resolver fixes apply on update instead of incremental hashing
  skipping unchanged files and serving stale data. Release builds key this off the
  commit; dev and dirty builds key off the binary's mtime so each local rebuild
  also reparses.
- Tree-sitter extraction for Go, Rust, Java, Ruby, C#, TypeScript/TSX, Lua,
  Python, JavaScript, Bash, Fish, C/C++, QML, CSS, SCSS, Markdown, JSON, YAML, TOML, INI, and Hyprland,
  plus a line-based reader for other config formats (sway/i3, rofi `rasi`,
  polybar, and similar). Markdown headings and JavaScript declarations become
  symbols, so docs and dashboard scripts are searchable by name as well as by
  content. Go, Rust, and TypeScript are indexed at the symbol level (functions,
  methods, types, structs/enums/traits/interfaces, and import edges), so `find`
  and `search` cover those projects; prowl can now index its own Go source.
- Go package graph: an in-module import resolves to every file of the imported
  package (synthetic `pkg` edges, rebuilt each resolve so they never accumulate),
  so `callers`, `callees`, `impact`, `changed`, `clusters`, and `entrypoints`
  work across a Go module. The module path comes from `go.mod`; standard-library
  and external imports stay informational and are not flagged. The fan-in/out
  health check excludes `pkg` so a widely-imported core package is not mistaken
  for a risk.
- TypeScript and JavaScript relative imports resolve to files: `./x` and `../x`
  try `.ts/.tsx/.js/.jsx/.mjs/.cjs` and an `index` file in a directory. A modern
  ESM/NodeNext import that cites a `.js` extension (`./foo.js`) resolves to its
  `.ts`/`.tsx` source, and non-source assets (`./styles.css`) match as-is, so
  `callers`, `impact`, `changed`, and `clusters` work across a TS/React app.
- TypeScript/JavaScript workspace packages: a bare import of a first-party
  monorepo package (`@scope/pkg`, `pkg/subpath`) resolves to that package's
  source by the `src/` convention (`src/index`, `src/<subpath>`, or its directory
  index), reading each `package.json` `name` to map the package to its directory.
  It deliberately ignores `package.json` `exports`/`main`, which point at built
  `dist/` output that is not indexed, so edges land on real source. ESM `.js`
  subpaths resolve to `.ts` the same way relative imports do. External (non-
  workspace) packages stay informational. On the tRPC monorepo this resolved 878
  previously-dangling `@trpc/*` imports and gave the core `@trpc/server` entry a
  real cross-package blast radius (impact `0` -> `345`); on zod it resolves the
  subpath imports (`zod/v4/core` -> `packages/zod/src/v4/core/index.ts`).
- Rust module graph: `mod foo;` declarations resolve to the included file
  (`foo.rs` / `foo/mod.rs`, handling both `mod.rs` and `foo.rs` parent layouts),
  `crate::` imports resolve to the module file under the importing file's crate
  root, and cross-crate `use other_crate::...` resolves to that workspace member
  (its crate name read from each `Cargo.toml`), falling back to the crate's
  `lib.rs` for re-exported items, so the graph works across a single crate and a
  Cargo workspace. `super::`/`self::` and external crates stay informational.
- Python imports resolve to files: absolute (`import a.b`, `from a.b import c`)
  to `a/b.py` or `a/b/__init__.py` (also under `src/`), and relative
  (`from .x import y`, `from ..pkg.mod import z`) against the importing file's
  package, so the graph works across a Python package. A bare `from . import x`
  and third-party imports stay informational.
- C# namespace graph: a `using Foo.Bar;` resolves to every file declaring
  `namespace Foo.Bar` (synthetic `pkg` edges, like Go), so `callers`, `impact`,
  `changed`, and `clusters` work across a C# solution. Framework and third-party
  usings match no declared namespace and stay informational.
- Java import graph: `import com.foo.Bar` resolves to that class's file under any
  module's source root (the part after `src/main/java`, `src/test/java`, or
  `src/`), so a multi-module Gradle/Maven project links across modules; wildcard
  `import com.foo.*` fans out to every file in the package. JDK and third-party
  imports stay informational. On the retrofit repo this took resolved edges from
  105 to 704 and produced real per-module clusters and cross-module blast radius.
- A graph of how files connect: include trees, exec and keybind to script chains,
  and shared color/font/path/variable references, with path and name resolution.
  Bare commands resolve against the project's command files by basename.
- QML graph resolution: component instantiations (`Foo { }`) resolve to the
  defining `Foo.qml` (same directory, then repo-unique stem, then nearest path),
  so the QML UI now forms its own subsystems instead of being invisible. Built-in
  and external types (QtQuick/Quickshell) are dropped rather than left dangling.
  `clusters` now reports each subsystem's dominant language. On a QML-heavy rice
  this took resolved edges from 228 to ~1950 and surfaced the 498-file QML shell
  as the top cluster (previously zero QML clusters).
- `clusters`/`overview` stay a useful subsystem map on cohesive codebases: when
  connected components collapse nearly everything into one giant blob (common for
  a well-linked Go/TS project), the blob is subdivided by directory subsystem
  (e.g. `pkg/gui`, `pkg/commands`) instead of shown as one opaque cluster. Config
  repos with several balanced include-tree components are left as-is. Clusters are
  labeled by directory subsystem (up to two segments), so a monorepo's
  `packages/foo` and `packages/bar` read distinctly instead of both as `packages`.
- SQLite store with FTS5 full-text search and an in-memory BFS blast-radius
  traversal (loads the resolved edge set once and visits each file once), in WAL
  mode so the indexer can write while the server reads. On a 2023-file Go repo a
  blast-radius query runs in ~80 ms.

#### Interface

- Shell query interface (the recommended, lowest-overhead path): `find`, `search`
  (`--smart`/`--compact`), `overview`, `clusters`, `callers`, `callees`,
  `relations`, `impact`, `entrypoints`, `references`, `hotspots`, `violations`,
  `tests`, `changed`, and `doctor` are first-class subcommands. Any agent that
  can run a command can use the index over a plain shell call: no MCP server, no
  `serve`, and none of the upfront tool-schema tokens MCP injects into every
  request. Each call resolves the workspace by walking up to `.prowl/` and
  freshens the index first, so results are current. A cheap file fingerprint
  (paths plus mtimes, no content reads) lets repeated calls skip the read-and-hash
  re-index when nothing changed, keeping the shell path fast on large repos.
- `find` matches an exact symbol name first, then full-text, then a substring
  fallback, so a camelCase or snake_case fragment (`cloud`) still surfaces the
  symbols that contain it (`updateCloudClient`) instead of returning nothing.
- `references <id>` answers "what uses this symbol": config/resource reference
  edges when present, and for code symbols (which have no language call graph) it
  falls back to full-text call sites of the name, excluding the definition. So
  `find <name>` then `references <id>` returns the call sites instead of nothing.
- `callees` lists what a file directly imports/execs/binds, not the package
  fan-out: a Go file importing N packages shows N imports, not one row per file
  in each imported package. On a 47-import file this is ~50 rows, not ~400.
  `callers` keeps the fan-out (so cross-package importers still show up). Both,
  plus `relations` and `references` edges, emit a uniform `{file, kind, line,
  raw, resolved}` row and drop internal node ids/types, so the result stays a
  TOON table even when a file mixes resolved and external edges (instead of
  degrading to the verbose per-item form).
- `changed` maps your git changes (working tree vs `HEAD`, or `--base <ref>`) to
  the files each one could affect, summarized per file (dependent count, subsystem
  breakdown, direct importers) like `impact`, so an agent can see the impact of an
  edit before committing without a flood of rows. `--all` includes unindexed paths.
- `impact` summarizes the blast radius by default: a total dependent count, a
  breakdown by subsystem (which surfaces the dependency hubs driving the radius),
  and the direct importers, instead of dumping every dependent. On a large Go
  package this is a 12-line summary rather than a 600+ row list. `--all` lists
  every dependent file.
- `overview` is a compact project map for an agent's first call: per-cluster it
  reports the label, language, and file count rather than every file path (the
  full lists stay in `clusters`), and it surfaces the project's architecture docs
  (root README, ARCHITECTURE, CONTRIBUTING, `docs/**` guides) so the agent reads
  the human-written guide before code. On a 2023-file Go repo this is a ~1.3 KB
  map instead of ~49 KB of mostly file paths.
- `clusters` lists subsystem summaries (label, language, file count) by default,
  and `clusters <name>` returns the files of matching subsystems, so pulling one
  subsystem stays cheap instead of dumping every cluster's files. On a 2023-file
  Go repo the summary is ~0.3 KB instead of ~47 KB.
- Savings are tracked on every delivery path: each shell query, like each MCP
  call, records what it returned versus the size of the files it pointed at, so
  `prowl-agent status` keeps counting when agents use the CLI instead of MCP.
- Token-lean output: query results default to TOON (Token-Oriented Object
  Notation), which encodes uniform result arrays as one header plus CSV-style
  rows. Models read it about 40% cheaper than JSON, and slightly more accurately.
  `--json` switches any query command back to JSON, and `--limit N` caps a result
  list so an agent can ask for the top N and pay for fewer tokens. Symbol results
  carry both `line` and `end_line`, so an agent reads just the symbol's range
  instead of the whole file.
- 17 MCP tools: `overview`, `clusters`, `find_symbol`, `find_references`,
  `find_callers`, `find_callees`, `file_relations`, `blast_radius`,
  `entrypoints_for`, `tests_for`, `similar_code`, `smart_search`,
  `architecture_violations`, `repo_hotspots`, `doctor`, `status`, and `reindex`.
- CLI surface: the setup and maintenance commands `init`, `status`, `doctor`,
  `restart`, `update`, and `version`, plus the read-only query commands above.
  The MCP `serve` and editor `lsp` commands stay hidden because agents and
  editors launch them over stdio; you never run them by hand. `init` is the single
  setup command and is idempotent: re-running it (after a reboot, say) keeps your
  settings instead of re-prompting.
- `restart` rebuilds the structural index from scratch and stops running serve/lsp
  processes so the agent or editor relaunches the current binary; the relaunched
  server re-embeds lazily, so an Ollama or model issue cannot block the restart.
- Setup writes MCP configs: the standard `.mcp.json` (most agents), Cursor, VS Code, Oh My
  Pi (`.omp/mcp.json`), Factory droid (`.factory/mcp.json`), and OpenCode
  (`opencode.json`, its own shape), plus an `AGENTS.md` block; state stays in a
  gitignored `.prowl/` folder. Server entries now include `type: "stdio"` (which
  VS Code requires). The README documents the one-command setup for any other agent.
  The `AGENTS.md` block is delimited by `<!-- prowl-agent -->` markers and only
  that span is ever rewritten, so a re-init refreshes prowl's guidance while
  leaving the user's own AGENTS.md untouched; a malformed block missing its
  closing marker replaces only the marker line, never the user text below it.
- Automatic freshness (no daemon, no extra command): the MCP server re-indexes
  right before a request when a change is pending, keeps a featherweight fsnotify
  watcher active for 30 minutes after each call, then suspends and resumes (with a
  catch-up re-index) on the next call, so agents never read stale data. `lsp` also
  re-indexes on save while it runs.

#### Editor (LSP)

- A language server (`prowl-agent lsp`) serves the same index to editors:
  go-to-definition (keybind to script, include to file, variable to declaration),
  find-references, hover with use counts, document and workspace symbols, code
  lens, completion of known variables and colors, and inline `doctor` diagnostics.
- `init` wires it up: editor configs under `.prowl/editor/`, plus a project-local
  `.helix/languages.toml` when there is none to overwrite; Neovim attaches it
  additively alongside your other servers.

#### Install, status, and updates

- One-line installer (`install.sh`) that downloads, checksum-verifies, and drops
  the binary in `~/.local/bin`.
- `prowl-agent update` replaces the running binary with the latest published build
  (downloaded and checksum-verified), then stops running serve/lsp processes so the
  agent or editor relaunches the new build, so it goes live without a manual
  restart. `prowl-agent status` shows update status ("up to date" or "update
  available") by comparing the build's commit against the latest commit on main
  (cached briefly, recomputed against the running build so it is correct right
  after an update); it works for source builds too via the embedded VCS revision.
- Redesigned `prowl-agent status`: a bordered card (in a terminal) with index
  stats, a language breakdown, and a token-savings report. Savings are measured
  per answer (the bytes each answer returned versus the size of the files it
  pointed at), kept at a conservative ~70% so the figure under-counts rather than
  over-claims. It aggregates across every initialized project for a combined total
  and links to `docs/TOKENS.md` so users can reproduce the measurement. Plain text
  when piped; `--json` carries the numbers.

#### Health checks

- `doctor`, as both a command and an MCP tool, reports cyclic includes,
  fan-in/out risk, oversized files, duplicate keybinds, broken commands, orphan
  scripts, dangling references, hardcoded colors, forbidden layer crossings, and
  git-churn hotspots, with a 0-100 score. The command renders a colored card in a
  terminal (score bar, severity breakdown, per-check counts, grouped findings) and
  plain text when piped; `--json` carries the report.
- Tuned to keep false positives low: references are emitted only for path-shaped
  targets, commands resolve against the project, checks skip lifecycle directories
  (migrations, installers, CI, vendor), and only project-relative broken includes
  are flagged. On one 2172-file project this brought the report from 2052 findings
  down to about 90, most of them real.
- `hotspots` (command and the `repo_hotspots` tool) ranks the largest functions
  by line span and the most complex functions by cyclomatic complexity, alongside
  central and large files: triage for where bugs hide. Rankings cover the
  project's own code; vendored and generated files (`vendor/`, `third_party/`,
  `node_modules/`, `*.pb.go`, ...) stay indexed and queryable but are excluded
  from hotspots as noise. Complexity counts decision points
  (if/for/while/switch-cases/catch/match-arms) in a function body for Go,
  Rust, C++, Java, Ruby, C#, TypeScript/TSX, JavaScript, and Python; other languages report size only.

#### Semantic search (optional)

- A local Ollama layer stores chunk embeddings in sqlite-vec. `similar_code` fuses
  vector and full-text results, and `smart_search` adds a query rewrite and a
  rerank. Both fall back to full-text when the layer is off. The model only
  reorders and rewrites; it never makes decisions and is never its own tool.
- Resilience: if the configured embed model is missing or Ollama is unreachable,
  the server logs a notice and runs structural-only instead of failing to start,
  and an embedding error during a refresh never fails the index.
- `init` sets up the semantic layer end to end: it offers model tiers (`fast` /
  `smart` / `max`, or `--tier`), prefers models already installed on Ollama over
  the tier preset (so it never points the config at an absent model or asks for a
  redundant pull), offers the official Ollama installer, brings the daemon up
  (reusing a service, installing a user service that survives reboot, or spawning
  it), pulls only missing models, and warms the embed model. AI on/off and tier
  persist in a global config so a fresh project inherits your last choice and a
  re-init never silently disables AI; `--reconfigure` re-opens the prompts. Tier
  presets track current best local models: `qwen3-embedding` for retrieval,
  `embeddinggemma` on the fast tier, `gemma4:e2b`/`e4b` for the smart/max assist,
  and `gemma3:1b` for the fast assist.

#### Build and docs

- CI builds a Linux x86_64 binary (cgo + FTS5) on every push to `main` and
  publishes it, with a checksum, to the rolling `nightly` release.
- README and an architecture write-up in `docs/ARCHITECTURE.md`.

### Notes

- Linux only for now; requires cgo and the `sqlite_fts5` build tag.
- The current focus is dotfiles and configs. Broader language support, including
  web and more scripting languages, is in progress.
- Works in any repo, not just `~/.config`. It indexes what git tracks and keeps
  its index in a gitignored `.prowl/`; agents read your real files, never
  `.prowl/`, so the gitignore does not hide code from them.
- `tests_for` is best-effort and marked `limited`, since configs rarely have
  formal tests.
