# Context packets and MCP v2

Prowl builds one context packet contract for the CLI, MCP clients, and future human interfaces. Retrieval is local and deterministic by default; no model or provider is required.

## Context packet contract

Use the CLI directly:

```bash
prowl context search "how are authentication tokens validated?" --mode compact --budget-tokens 1800 --json
prowl context get concept:authentication --mode standard --budget-tokens 1800 --json
```

A packet includes:

- schema version `prowl.context/v1`, a deterministic summary, and stable item IDs;
- compact, standard, or explicitly bounded full detail;
- estimated token cost through a replaceable estimator and exact packed byte cost;
- selection reasons, freshness, confidence, and audience;
- citations and a `detail_resource` for progressive retrieval;
- omission counts such as `budget`, `duplicate`, and `not_found`;
- a trace ID whose persisted record contains metadata only.

Token counts are estimates, not tokenizer parity. Packed byte counts and byte-budget enforcement are exact for the returned title/summary/content payload. `full` requests must still specify a token or byte budget.

Retrieval combines:

1. lexical matches from canonical OKF knowledge;
2. lexical matches from indexed source chunks;
3. a capped one-hop dependency expansion in both directions;
4. freshness, confidence, and curated-knowledge factors;
5. diversity-aware packing under the requested budget.

When workspace AI is enabled and the configured local inferencer is reachable, CLI and MCP context services wire the existing provider-neutral rerank operation into candidate scoring. A missing, disabled, timed-out, malformed, or failing reranker leaves deterministic lexical/graph ordering unchanged.

Changed source anchors lower stale knowledge while preserving it with an explicit warning and current-source fallback.

## MCP surfaces

`prowl serve` retains the historical surface by default:

```bash
prowl serve --mcp-surface legacy  # default: 17 compatible tools
prowl serve --mcp-surface core    # six intent-oriented tools
prowl serve --mcp-surface all     # legacy plus core
```

The core tools are:

- `search_context`
- `get_context`
- `analyze_change`
- `propose_knowledge_change`
- `validate_knowledge`
- `search_capabilities`

Read-only tools carry read-only/closed-world annotations. Core results combine typed structured output with first-class MCP resource links. Proposal creation is additive, creates only a review inbox entry, and never accepts canonical knowledge.

### Resources

Fixed resources:

- `prowl://workspaces`
- `prowl://workspace/current/overview`
- `prowl://workspace/current/knowledge/index`
- `prowl://workspace/current/changes`

Templates:

- `prowl://workspace/current/concept/{id}`
- `prowl://workspace/current/source/{+path}`

Packet source paths are escaped as URI path segments, including slashes and reserved characters. Reads reject lexical traversal, encoded traversal, symlink escape, non-regular files, and files larger than 2 MiB. Resource annotations include audience, priority, and available filesystem modification time.

### Prompts

The server publishes bounded, progressive-disclosure prompts:

- `understand-project`
- `research-topic`
- `review-change`
- `update-knowledge`
- `prepare-implementation`

They instruct clients to begin with overview/index or compact context, then fetch only selected detail resources.

### Optional host capabilities

`search_context` accepts `synthesize: true`. If the client advertises MCP sampling, Prowl requests a summary of a bounded compact projection with a 160-token response limit. Sampling can replace only the packet summary; it cannot alter selected evidence, citations, ranking, budgets, or traces. Unsupported capability, errors, non-text output, and empty output all fall back to the deterministic summary.

For proposal creation, an elicitation-capable client is always asked for human confirmation. Decline, cancel, a missing confirmation field, or elicitation failure blocks the write. Clients without elicitation cannot create proposals through MCP; a human must use `prowl knowledge propose` locally. Ordinary tool arguments are never treated as proof of human approval. Neither path accepts the proposal. Acceptance verifies the proposal's base hash and snapshots the canonical document, index, and log before mutation. A later failure triggers rooted restoration of every snapshot; any restoration failure is joined into the returned error rather than hidden.

## Capability discovery

Built-in manifests expose metadata before workflow details:

```bash
prowl capabilities search context
prowl capabilities get retrieve-context
```

Search is deterministic weighted lexical matching over names, titles, descriptions, and triggers. Manifests declare tools, resources, outputs, privacy, read/write behavior, and version.

## Privacy-safe traces

Context runs store only:

- a workspace-salted HMAC of the question or requested IDs;
- mode and requested/estimated budgets;
- selected IDs and aggregate omission counts;
- aggregate timing, strategy version, status, and error class;
- creation time and trace ID.

Question text, snippets, source bodies, sampled replies, provider payloads, and secrets are not stored. Aggregate trace rows are retained for 30 days and pruned best-effort during later trace writes. Tracing or cleanup failure never fails retrieval. `exact_bytes` is the exact UTF-8 byte count of packed evidence fields (title, summary, and selected content); it intentionally excludes JSON/TOON transport framing and packet metadata.

Inspect the safe projection with:

```bash
prowl context traces --limit 20
prowl context traces --limit 20 --json
```

## Deterministic retrieval evaluation

The checked-in corpus is `internal/context/testdata/retrieval-corpus.json`. It contains seven cases: three identifier lookups, three natural-language questions, and one low-signal down-weight case. Most declare two expected sources (including a dependency-neighbor source), one expected concept, a known distractor, required compact-packet evidence, and an 800-token budget; the low-signal case declares one real source against a keyword-dense generated-locale distractor under a tight 120-token budget.

Run the acceptance evaluation:

```bash
go test -tags sqlite_fts5 ./internal/context \
  -run TestRetrievalEvaluationImprovesHitsUnderFixedBudget -count=1 -v
```

Observed on the checked-in seven-case fixture:

| Strategy | Mean recall | Mean distractor-aware precision | Mean required-evidence utilization | Complete evidence | Exposed retrieval operations |
|---|---:|---:|---:|---:|---:|
| scripted grep/file agent | 0.52 | 0.74 | 0.79 | 1/7 | 5.4 |
| scripted existing-Prowl agent | 0.52 | 0.79 | 0.79 | 1/7 | 2.9 |
| lexical packet | 0.52 | 0.79 | 0.79 | 1/7 | 1 |
| graph packet | 0.72 | 0.86 | 1.00 | 1/7 | 1 |
| hybrid knowledge/source/graph | **1.00** | **0.89** | **1.00** | **7/7** | **1** |

All packets stayed under each case's fixed token estimate; hybrid packets used 120 to 179 estimated tokens in this fixture. Operation counts are instrumented by a deterministic scripted-agent harness: each grep/search invocation and each selected file read is counted, while a complete context packet is one tool invocation.

`Complete evidence` means the scripted agent received all declared sources/concepts and required terms. This provider-free benchmark measures the plan's source-hit and tool-operation exit criteria without making an LLM-quality claim; provider-specific agent evaluation remains an optional integration benchmark.

The low-signal case reproduces a real failure: a keyword-dense generated locale table (`i18n/catalog_gen.go`) scores higher on raw lexical overlap than the real `bar/battery.go` widget. The ranker dampens the lexical contribution of low-signal file classes (locale/i18n, generated, lockfiles, minified bundles) unless the query itself names the class, so the real code is surfaced and the distractor is dropped under budget. Prose documentation (Markdown and friends) is dampened more mildly, since a doc can be the answer but should rank below code for a code question. A direct identifier match to such a file is never dampened.

Run timing benchmarks separately:

```bash
go test -tags sqlite_fts5 ./internal/context -run '^$' \
  -bench '^BenchmarkRetrievalStrategies$' -benchtime=100x -count=1
```

One Linux AMD64 run measured roughly 86 µs for lexical/current FTS, 179 µs for graph, and 300 µs for hybrid. Timing is machine-dependent; the acceptance gate is the deterministic quality/budget comparison above.
