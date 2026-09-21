# Durable knowledge and OKF

Prowl stores accepted project knowledge as portable Markdown under `.prowl/knowledge/`. SQLite is a derived search/index cache: deleting `.prowl/index.db` never deletes accepted concepts, decisions, claims, or playbooks.

## Layout

```text
.prowl/
  config.toml       # trackable settings
  rules.toml        # trackable policy
  knowledge/        # canonical OKF bundle; track this in Git
    index.md
    log.md
  proposals/        # review inbox; optionally track this in Git
  index.db*         # derived and ignored
  cache/            # derived and ignored
  logs/             # derived and ignored
```

Older repositories may contain a broad `.prowl/` ignore. Prowl preserves that user-visible line and appends marker-owned negations so settings, knowledge, and proposals remain trackable. It never silently removes an unowned ignore rule.

## Start a bundle

```bash
prowl knowledge init
prowl knowledge list
```

A concept is an OKF v0.1 Markdown file:

```markdown
---
type: Decision
title: Keep accepted knowledge in Markdown
resource: file://docs/architecture/storage.md
tags: [architecture, storage]
timestamp: 2026-07-23T00:00:00Z
prowl:
  id: decision-storage
  status: accepted
  confidence: verified
  anchors:
    - path: internal/store/store.go
      line_start: 28
      line_end: 72
      content_hash: sha256:...
---

SQLite is rebuildable. Accepted reasoning is not.
```

Unknown OKF types, top-level fields, nested `prowl:` fields, and YAML scalar types are retained. `index.md` and `log.md` may omit `type`. Missing indexes and broken links are lint findings rather than import failures.

## Review proposals

Proposal ingestion is deterministic and requires no model:

```bash
prowl knowledge propose \
  --file /tmp/candidate.md \
  --target decisions/storage.md \
  --author codex

prowl knowledge accept <proposal-id>
# or
prowl knowledge reject <proposal-id>
```

`propose` validates the candidate and prints a deterministic diff without modifying accepted knowledge. `accept` prints the same diff, writes atomically, appends `log.md`, refreshes the marker-owned index, and only then marks the proposal accepted. Destination collisions are refused. A failed later write restores the prior accepted document, index, and log.

Instead of authoring the OKF file, pass the fields and prowl assembles it:

```bash
prowl knowledge propose \
  --type Claim \
  --title "Foo guards empty input" \
  --body "Foo returns early when input is empty." \
  --anchor internal/foo/foo.go#Foo \
  --target claims/foo.md
```

`--anchor` takes `path#symbol` (tracks the symbol) or `path:start-end`. The same
fields are available on the `propose_knowledge_change` MCP tool. A candidate that
puts a prowl field (`anchors`, `status`, `confidence`, `related`, `valid_from`,
`valid_to`) at the top level instead of under `prowl:` is rejected rather than
silently ignored.

## Inspect and validate

```bash
prowl knowledge show decision-storage
prowl knowledge show decisions/storage.md
prowl knowledge lint
prowl knowledge export ./knowledge-export
```

Every command that returns structured records supports `--json`.

Lint findings use stable namespaced codes and include:

- `knowledge.invalid_document`
- `knowledge.duplicate_id`
- `knowledge.contradiction`
- `knowledge.orphan`
- `knowledge.broken_link`
- `knowledge.invalid_resource`
- `knowledge.invalid_temporal_range`
- `knowledge.stale_anchor`
- `knowledge.moved_anchor`
- `knowledge.missing_anchor`
- `knowledge.invalid_anchor`
- `knowledge.missing_evidence`

A source anchor pins a claim to code in one of two ways:

- **Line range** (`line_start`/`line_end`, 1-based inclusive). A line inserted
  above the region shifts it, but the anchored lines are still found by content
  and reported as `moved_anchor` with the range they occupy now, not as stale
  evidence. `stale_anchor` is reserved for the case that actually matters: the
  anchored lines themselves changed.
- **Symbol** (`symbol: <name>`). The symbol's range is re-resolved from the index
  on each `propose` and `lint`. The anchor follows the symbol when lines move
  above it and goes stale only when the symbol's body changes. If the symbol name
  stops resolving but its body is untouched -- a rename -- the anchor is still
  recovered by content and reported as `moved_anchor`.

Anchor hashes are SHA-256 over the region; CRLF and LF normalize to LF, and the
final newline does not affect the digest. Omit `content_hash` in a candidate and
`propose` computes it from the current source. An unreadable path or an
unresolved symbol is reported by lint.

`prowl knowledge lint --repair` rewrites the line range of every anchor
reported as `moved_anchor`, then reports what remains. Only the coordinates
change: the stored `content_hash` still describes the same lines, so a repaired
anchor stays verifiable rather than merely silenced. Genuinely changed, missing,
and invalid anchors are never touched by `--repair`; they need a human decision
about whether the claim still holds.

Relocation searches outward from the recorded position and takes the nearest
match, so a region that legitimately repeats in a file (boilerplate, table rows)
resolves to one deterministic answer. The search is bounded, so a pathological
large-region-in-large-file case reports `stale_anchor` rather than stalling a
lint pass.

## Migration safety

Opening an older on-disk index creates a self-contained SQLite backup using `VACUUM INTO` before applying the transactional migration. Future schema versions are refused without modification. `store.RestoreBackup` replaces a closed database and retains the previous database as `index.db.restore-previous` for reversal.

These safeguards protect derived state. Canonical knowledge remains independently recoverable from `.prowl/knowledge/` and Git.
