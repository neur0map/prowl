# Native large-change review: design

Prowl Agent will gain a native, delegation-first review subsystem for commits,
branch ranges, and working-tree changes. Prowl will deterministically capture and
partition a change, retrieve graph-aware repository context, and verify review
coverage. A host coding agent will perform the semantic review. Prowl will not
become a hosted reviewer, choose an LLM provider, or claim that deterministic
coverage validation proves a finding is semantically correct.

The subsystem is mandatory in structured mode when a text change has more than
300 raw changed lines. This is a strict threshold: 301 additions plus deletions
activates structured review; 300 does not.

## Problem

Prowl already provides the ingredients needed to understand the impact of a
change:

- an incrementally refreshed structural index;
- symbols and exact source spans;
- dependency relations and subsystem clusters;
- callers, references, entrypoints, tests, and blast radius;
- bounded context packets with citations; and
- CLI-first integrations used by coding agents.

Those ingredients are exposed as independent queries. A reviewer facing a large
change still has to decide which files belong together, what order to read them
in, which surrounding code matters, whether every hunk was examined, and whether
the reviewed head moved. Large language models are unreliable at doing that from
a flat diff: they silently skip files, lose information in long contexts, split
related changes, and produce findings with stale or invented locations.

The existing GitHub composite action is not a semantic reviewer. It initializes
Prowl, runs `doctor`, and emits SARIF. Its naming and comments currently imply a
broader review than it performs. The existing `changed` command reports changed
files and their blast radius but does not capture patches, group changes, produce
bounded review territories, or validate review coverage.

## Goals

- Make `prowl review` the native entry point for reviewing a workspace,
  commit, or branch/PR range.
- Require Prowl's structured protocol whenever raw text additions plus deletions
  exceed 300 lines.
- Turn a flat diff into dependency-aware cohorts, ordered layers, and bounded
  symbol-aligned review units.
- Account for every changed path and every reviewable hunk, including deletion-
  only and mechanical changes.
- Give a host LLM the exact patch plus the smallest useful amount of cited
  repository context.
- Require explicit coverage receipts and cross-cutting audits before an agent may
  report an all-clear or approval recommendation.
- Detect stale reports and line-location drift deterministically.
- Keep the core local, read-only with respect to project source, model-agnostic,
  and usable from any coding agent that can invoke the CLI.
- Prove that the treatment improves large-PR review quality under a fixed model
  budget before installing the mandatory routing rule.

## Non-goals

- Hosting a GitHub/GitLab application or network service.
- Selecting, authenticating, or billing an LLM provider.
- Posting provider-native review comments in the first release.
- Automatically fixing findings.
- Detecting whether a change was authored by an LLM. Structured review assumes
  any large change may be machine-generated instead of using unreliable style
  heuristics.
- Executing builds, tests, scripts, hooks, or code from the reviewed revision.
- Replacing `doctor`, linters, type checkers, tests, or security scanners.
- Generating architecture diagrams or a graphical review interface.
- Reusing semantic findings across unrelated reviews.
- Adding a permanent MCP core-tool schema in the first release. The CLI and
  capability catalog are the canonical agent path.

## Research basis

The design adopts the parts of existing systems that fit Prowl's architecture:

- CodeRabbit Change Stack's change cohorts, dependency-ordered layers, range
  summaries, and explicit navigation order.
- OpenCodeReview's separation of deterministic selection and bundling from LLM
  reasoning, including a host-agent delegation mode.
- Qwen Code's coverage receipts, removed-behavior audit, caller/producer tracing,
  base-revision review rules, incomplete-review disclosure, and immutable finding
  artifact.
- PR-Agent's symbol-aware dynamic context and post-generation reflection.
- PR-AF's adversarial verification and gap review.
- Reviewdog's separation between finding data and provider-specific rendering.

The design deliberately rejects several patterns:

- PR compression that removes deletion-only hunks or silently omits patches.
- One prompt containing the entire large diff merely because it fits a context
  window.
- LLM-generated file grouping when Prowl already has a deterministic graph.
- Numeric confidence as if an LLM probability were calibrated.
- Unbounded reviewer fan-out, reverse-audit loops, or multi-model voting.
- Treating repository content or PR metadata as trusted instructions.

## Definitions

### Scope

A review scope is one of:

- **workspace**: tracked staged and unstaged changes against `HEAD`, plus every
  non-ignored untracked path returned by Git's NUL-delimited untracked-path
  enumeration;
- **commit**: one non-merge commit against its first parent, or a root commit
  against Git's empty tree; merge commits require an explicit range; or
- **range**: the unique best merge base of `--base` and `--head` through
  `--head`.

`--head` defaults to `HEAD`. Refs are resolved to full object IDs before any diff
is captured. Ref arguments beginning with `-`, non-commit objects, missing
objects, and ambiguous merge-commit scopes are rejected. Range resolution asks
Git for every best merge base and requires exactly one; zero or multiple best
merge bases fail with a precise error rather than selecting an arbitrary patch.

Git does not track directories, so an untracked directory is represented by the
paths Git enumerates beneath it. Regular files and symlinks can produce
synthesized patches. Sockets, devices, FIFOs, and other special paths are listed
as unreviewable and are never opened or followed.

### Churn

Threshold accounting is independent of repository-controlled diff attributes.
Prowl first obtains changed paths, modes, and object IDs without textconv or
external diff. `ThresholdTextV1` classifies the raw bytes of every existing
regular-file or symlink side. A side is recognized text when Prowl recognizes
its path as an indexed text language/config format or its first
`min(8000, size)` bytes contain no NUL. A path is text when either side is
recognized text; otherwise it is binary when any side contains a NUL in that
prefix. Symlink blobs use their exact target bytes, including malicious committed
blobs containing NUL. Regular-to-symlink and symlink-to-regular transitions diff
the two raw side blobs under the same rule.

Text paths are diffed from their raw base/head bytes with sanitized,
attribute-free `git diff --no-index --text` invocations over private temporary
files; rendered paths are replaced with the canonical repository paths. This
prevents `.gitattributes` values such as `-diff`, textconv, custom drivers, or
working-tree encodings from suppressing threshold churn.

- `raw_additions` and `raw_deletions` are counts of `+` and `-` payload lines in
  those forced-text patches, excluding `+++` and `---` headers.
- An empty added file has zero additions. Otherwise an all-addition untracked
  blob has `LF count + 1` lines when its final byte is not LF, or `LF count`
  lines when it ends in LF. CR is an ordinary payload byte, so CRLF is one line.
  Symlink-target blobs use the same rule.
- `raw_churn = raw_additions + raw_deletions`.
- Binary files, gitlinks, and special paths are counted as changed paths but have
  no line churn.
- `reviewable_churn` counts additions and deletions in hunks Prowl can present as
  text review units.
- `structured_required = raw_churn > 300`.

Patch-presentation limits never change threshold accounting. Exact untracked
accounting has separate safety bounds: 64 MiB per regular file and 512 MiB total
per plan. Exceeding either bound, timing out, or failing to reach EOF fails plan
construction; Prowl never estimates or truncates `raw_churn`.

Generated files, vendored files, lock files, documentation, tests, and other
mechanical changes count toward `raw_churn`. Their review role may differ, but
none may disappear from scope accounting.

### Review unit

A review unit is the smallest bounded territory that a host agent reviews and
acknowledges. It owns one or more complete diff hunks and, where available, maps
them to enclosing symbols in the base and head revisions.

### Cohort and layer

A cohort is a logically related group of changed files and units. A layer is a
review order within a cohort. Dependencies and contracts appear before their
consumers; tests and operational material follow the behavior they exercise.

### Receipt

A receipt is the host agent's machine-readable declaration that it examined a
unit or required cross-cutting audit. A receipt records covered ranges, context
consulted, findings, and any remaining uncertainty. It is evidence of process
coverage, not proof that the model noticed every defect.

## User experience

The command group is:

```sh
# Capture current staged, unstaged, and untracked work
prowl review plan

# Capture PR-style branch changes
prowl review plan --base main --head HEAD

# Capture one non-merge commit
prowl review plan --commit abc123

# Fetch one bounded territory from a persisted plan
prowl review unit <review-id>/<unit-id>

# Validate the agent's canonical report
prowl review check --review <review-id> --report review-results.json
```

Every command supports the root persistent output formats. TOON is the default
when piped; JSON is the stable machine contract; human and Markdown formats are
for inspection.

The machine contract separates mode from obligation:

- `mode` is exactly `direct` or `structured`;
- `structured_required` is true exactly when raw churn exceeds 300; and
- `--structured` sets `mode=structured` below the threshold while leaving
  `structured_required=false`.

Without `--structured`, mode is direct at 300 lines or fewer and structured
above 300. No flag may set direct mode when `structured_required=true`.

Direct mode still returns scope, statistics, changed paths, attention signals,
and a single bounded unit when possible. It does not require the receipt matrix.
Structured mode returns cohorts, layers, primary units, audit targets, and the
receipt contract.

The plan ends with exact next commands for each unit and for `review check`.

## Package architecture

A new `internal/review` package owns the transport-independent domain:

- Git scope resolution and safe command execution;
- numstat, raw patch, rename, binary, gitlink, and untracked-file parsing;
- base/head source snapshots for changed files;
- hunk-to-symbol mapping;
- file roles, cohorts, layers, and attention signals;
- review-unit context compilation;
- plan persistence, identity, and staleness checks; and
- report/receipt validation.

`internal/application.Project` gains a `Review` service assembled from the
workspace root, project configuration, store, querier, and context service. The
review service may reuse query/store APIs but query packages do not import the
review domain.

`internal/cli/review.go` owns Cobra wiring and presentation only. It does not run
Git or construct plans directly.

The capability catalog gains a `review-large-change` manifest that points agents
to the CLI workflow. A portable `skills/prowl-pr-review/SKILL.md` becomes the
canonical host-agent protocol. Setup installs the skill and adds a compact
routing rule to generated `AGENTS.md` and OMP's sticky rules.

No LSP surface is added. No MCP core tool is added in v1. An MCP client can
discover the capability and invoke the named CLI commands through its host.

## Snapshot and Git safety

All Git subprocesses use argument arrays, explicit working directories, context
cancellation, bounded output, and a fixed timeout. No shell is involved. Name
and object data use NUL-delimited formats wherever Git supports them.

Every invocation runs with a sanitized Git execution policy. Prowl disables
pagers, interactive prompting, optional locks, external diff commands, textconv,
filesystem monitors, repository hooks, credential helpers, and lazy promisor
fetches; sets an empty hooks directory; sets `GIT_NO_LAZY_FETCH=1`; and denies
all remote protocols/helpers for object reads. A missing local object fails
instead of fetching it. System/global Git configuration is excluded.

Prowl enumerates filter-driver names from local configuration without invoking
them and overrides every clean, smudge, process, and required key before a
command can inspect worktree content. Every output-affecting diff option is
pinned: histogram algorithm, indent heuristic off, three context lines, zero
inter-hunk context, no color, no textconv/external diff, fixed prefixes, rename
detection at 50%, unlimited rename limit, and no copy detection. Repository
attributes never classify threshold text or render canonical hunks. Tests install
hostile helper/config values, including alternative algorithms, heuristics,
context, rename limits, partial-clone remotes, and `ext::` transport, and prove
that canonical records stay identical and no helper starts.


Workspace mode uses the current refreshed Prowl project because the reviewed
content is the current workspace. Every workspace-side path read, tracked or
untracked, is opened relative to a pinned workspace directory handle. Regular
files use no-follow semantics for every path component and are verified from the
opened handle before reading; symlinks use no-follow `readlink`. A type or
identity change is concurrent modification, never a reason to follow a
replacement target.


Workspace capture is a verified transaction:

1. Record the resolved `HEAD`, canonical tracked patch, NUL-delimited untracked
   path set, and exact untracked content/link digests.
2. Refresh the project index and build every plan artifact from that capture.
3. Capture the same fingerprints again and refresh the index once more.
4. Accept the plan only when both captures and the published index signature
   agree. Otherwise retry the entire transaction once, then fail as concurrently
   modified.

Committed range/commit mode requires graph context for the resolved head, not
whatever branch happens to be checked out. If the current clean worktree is
exactly the resolved head, Prowl reuses its index. Otherwise Prowl materializes
the head directly from Git tree objects:

1. Enumerate the resolved tree with sanitized `git ls-tree -rz --full-tree`.
2. Read regular and symlink blobs through sanitized `git cat-file --batch`.
3. Write them into a private snapshot and build a private Prowl index there.

Direct tree/blob materialization deliberately avoids `git archive`, checkout,
and export attributes. The writer allows regular files, directories, and
symlinks, but rejects absolute paths, `..` traversal, duplicate paths, hard
links, devices, FIFOs, sockets, more than 250,000 entries, any regular blob above
64 MiB, and more than 4 GiB total. Symlinks are stored as links, never followed,
and may not escape the snapshot when opened by the indexer. No checkout filter,
hook, package installer, or project command runs.

Base-side changed-file content is read from Git objects. The existing language
detector and extractors parse base blobs when a deleted or modified hunk needs
its former enclosing symbol.

## Plan identity and persistence

Review state lives outside the worktree under:

```text
<git-common-dir>/prowl/reviews/<review-id>/manifest.json
```

Prowl resolves and safely opens Git's common metadata directory, creates a locked
private temporary review directory there, and atomically renames it after the
final review ID is known. Review artifacts therefore cannot enter workspace
status even when `.prowl/` is not ignored. Multiple linked worktrees share the
same collision and retention domain.

All identity encodings use `FramedFieldsV1`:

```text
field := uint16be(name_length) | ASCII_name |
         uint64be(value_length) | value_bytes
list  := uint64be(item_count) |
         repeated(uint64be(item_length) | item_bytes)
```

Field order is part of each schema below. Integers inside values are unsigned
big-endian 64-bit unless a field explicitly says raw bytes. Strings are UTF-8.
Sets are sorted by raw UTF-8 bytes. The implementation ships normative test
vectors for every identity schema.

An identity side is a framed `(kind, value)` pair. Kind is `git_oid`,
`workspace_sha256`, or `absent`. Commit/range base and head identities are raw
Git OIDs. Workspace base is the resolved `HEAD` Git OID; workspace head is
`workspace_sha256` over `WorkspaceTreeV1`, the sorted changed tracked/untracked
final path records `(path, mode, kind, content_sha256|absent)`. For each path
side, the canonical record similarly stores tagged identity. A workspace regular
or symlink side gets a computed Git blob OID in the repository object format
without writing the object; a missing side is `absent`.

`ScopeIdentityV1` encodes in order:

1. schema `review.scope.v1`;
2. Git object-format name (`sha1` or `sha256`);
3. tagged base identity;
4. tagged head identity;
5. scope mode; and
6. 32-byte canonical-patch digest.

The object format comes from `git rev-parse --show-object-format`; the empty-tree
OID is derived in that format. Other formats fail explicitly.

`CanonicalPatchV1` is a framed list of raw tracked/untracked path records sorted
by new path, old path, then status. Each record encodes in order:

1. record kind, status, old path, new path, old/new modes, and old/new tagged
   side identities;
2. threshold-text/binary classification and exact additions/deletions;
3. 32-byte raw old/new content digests when content exists; and
4. a framed list of canonical raw hunk records.

Each raw hunk record encodes ordinal within the path, old/new coordinates,
no-final-newline flags, exact payload bytes, and payload SHA-256. Untracked
records use the same fields with an absent old side. Changed paths must be valid
UTF-8 in v1; invalid paths fail with escaped raw bytes. Canonical raw hunk
payload is capped at 128 MiB total across at most 100,000 changed paths;
exceeding either bound fails the plan.

Plan-only classification is computed after the scope digest. Each path gets
review class `full`, `mechanical`, or `unreviewable` and aggregate coverage state
`full`, `partial`, or `none`. Text paths are full or partial; binary/special/
gitlink paths with no reviewable text are none. Each text hunk gets reviewability
`reviewable` or `unreviewable_large_text`.

`ReviewPlanIdentityV1` encodes in order:

1. schema `review.plan.v1`, planner version, and full scope digest;
2. index schema/version and head-index content signature;
3. ordered path IDs, review classes, coverage states, reasons, and role IDs;
4. ordered path-qualified hunk IDs, reviewability values, and reasons;
5. ordered primary units with kind, hunk IDs, and attention-signal IDs;
6. ordered cohort/layer IDs and member unit IDs;
7. ordered required-audit IDs and target IDs;
8. ordered trusted-local and untrusted-policy evidence content digests; and
9. planner constants, including threshold, unit-line cap, mandatory-JSON cap,
   text-classifier version, and context-ranker version.

Any project configuration that changes planning changes one of those encoded
outputs/constants. Formatting-only configuration changes that produce the same
plan do not change identity.

All content-derived public IDs store a full SHA-256 and expose a prefix plus the
first 16 digest bytes as 32 lowercase hex characters; a prefix collision with a
different full digest is an error. The schemas are:

- path `p_`: full scope digest plus canonical raw path record;
- hunk `h_`: full scope digest, full path digest, hunk ordinal, and raw hunk
  record;
- unit `u_`: ordered full path-qualified hunk digests plus unit kind;
- cohort `c_`: full scope digest, stable cohort label, and ordered member unit
  IDs;
- layer `l_`: cohort ID, topological ordinal, and ordered member unit IDs; and
- semantic audit target `t_`: target kind, path ID, side, symbol kind/name,
  source coordinates, and signature/content digest.

Primary coverage range IDs are hunk IDs. Removed-symbol, changed-signature, and
added-field/option targets use `t_`. Hunkless changes use path IDs. Required
audit IDs are fixed schema constants:
`audit_removed_behavior_v1`, `audit_contract_migration_v1`,
`audit_test_matrix_v1`, and `audit_integration_gap_v1`.

The full SHA-256 of `ReviewPlanIdentityV1` is stored in the manifest. The public
review ID is `rvw_` plus the first 20 digest bytes as 40 lowercase hex
characters, with the same collision check. Normative vectors include identical
same-coordinate edits in different files, hunkless paths, and every target type.

Workspace plans record verified final capture fingerprints and become stale as
soon as the canonical workspace scope changes. Commit and range reports apply to
their immutable resolved head OID regardless of checkout; a provider integration
compares its live PR head before posting.

Repeated creation of the same plan reuses the immutable manifest and snapshot.
On plan creation, Prowl removes review directories older than seven days and
keeps at most the twenty most recent plans. It never deletes a locked plan.

## Change capture

The provider first captures path status, modes, and object IDs with sanitized
raw Git commands. It then reads base/head bytes directly and builds the framed
`CanonicalPatchV1` records using `ThresholdTextV1` and attribute-free,
forced-text per-file diffs. It captures:

- file status and old/new paths, including renames detected at exactly 50%;
- exact additions and deletions;
- indivisible unified hunks with old and new coordinates;
- file mode changes;
- binary classifications;
- gitlinks/submodule object changes; and
- synthesized all-addition records for safely read untracked regular text files
  and link-target records for untracked symlinks.

Copy detection is disabled in v1. A copied file is an addition unless the fixed
rename rule pairs it with a deletion.

A changed path receives a review class:

- `full`: ordinary text code/configuration/test/documentation;
- `mechanical`: generated, vendored, lock, snapshot, or repetitive text whose
  transportable hunks remain visible with reduced surrounding context; or
- `unreviewable`: binary, unsafe/special, or unavailable gitlink content.

Each text hunk separately receives reviewability `reviewable` or
`unreviewable_large_text`. A text path's aggregate coverage state is `full` when
all hunks are reviewable and `partial` when at least one is oversized. Small
hunks in a partial path retain normal primary units; only oversized hunk target
IDs move to the integration/gap audit.

An untracked regular file above the exact-accounting bounds fails the plan before
mode selection; it is not converted into an estimated unreviewable entry.

Every tracked changed path and every path returned by Git's untracked
enumeration appears in the plan. Unreviewable paths and oversized hunk IDs carry
precise reasons and become integration/gap targets. Prowl never labels
unreviewable content safe.


## Hunk-to-symbol mapping

Unified Git hunks are indivisible primary-ownership atoms. For each text hunk:

1. Parse the base and head file when a supported extractor exists.
2. Record every enclosing/touched symbol on each side.
3. Classify a hunk wholly inside one before/after symbol pair as symbol-aligned.
4. Classify a hunk spanning declarations as one multi-symbol atomic hunk.
5. Coalesce adjacent whole hunks only when they share the same before/after
   symbol set.
6. Preserve deletion-only hunks and their base symbols when no head symbol
   remains.
7. Fall back to one atomic hunk when the language is unsupported or parsing
   fails.

`CanonicalUnitMandatoryJSONV1` is the exact size and machine-output schema. It is
compact UTF-8 JSON from a fixed-field struct, with HTML escaping disabled,
standard padded base64, no optional context, and one trailing LF. Fields appear
in this order:

1. `schema` (`review.unit.v1`);
2. fixed-length `review_id`, `unit_id`, `cohort_id`, and `layer_id` strings;
3. `scope_kind`, `object_format`, and tagged base/head identity objects; and
4. `hunks`, an ordered array whose fixed fields are path ID, old/new path,
   status, ordinal, old start/count, new start/count, old/new no-final-newline
   booleans, and exact `patch_base64`.

Sizing uses zero-filled placeholders of the final fixed ID lengths before the
review/cohort/layer IDs exist; replacing them cannot change serialized length.
The schema excludes reviewability, roles, signals, labels, symbols/outlines, and
all other plan-derived or optional metadata, so classification has no identity
cycle. The final machine response uses the same mandatory fields/serialization
and then separately bounded optional context.

`MaxUnitChangedLinesV1` is exactly 400, and
`MaxUnitMandatoryJSONBytesV1` is exactly 16 MiB. A changed line is a `+` or `-`
payload line excluding file headers; replacements count both sides. After
file-level cohort/layer assignment, atomic whole-hunk groups are partitioned by
`(cohort_membership_key, layer_ordinal)`. Each partition is ordered by path and
coordinates and greedily packed while both caps remain satisfied. A primary unit
may never cross a cohort or layer boundary. A hunk above 400 lines but within the
JSON cap becomes one `oversized_atomic` unit. A one-hunk mandatory JSON object
above 16 MiB becomes `unreviewable_large_text`; other hunks in that path remain
reviewable.


Human and Markdown output may show an explicitly non-canonical escaped preview,
but JSON and TOON carry the exact base64 value. Presentation limits apply only
to optional surrounding evidence. Every published unit returns the complete
mandatory object; plan construction proves it fits.

Each reviewable hunk is owned by exactly one primary unit. Overlapping contextual
snippets are allowed, but primary ownership is not.

## File roles

Roles are deterministic and non-exclusive metadata:

- contract/type/schema/migration;
- implementation;
- consumer/integration/entrypoint;
- test;
- configuration/build/deployment;
- documentation;
- generated/vendor/dependency/lock; and
- unknown/unindexed.

Roles come from indexed language/role metadata, symbol kinds, entrypoint and test
queries, recognized manifest/migration/config paths, and changed-file relations.
A role is an explanation and grouping signal, not a semantic finding.

## Cohort construction

Cohorts and layers are assigned at file/hunk-group level before final unit
packing, without an LLM:

1. Intersect changed paths with Prowl's existing subsystem clusters.
2. Join directly related changed files even when they cross a cluster boundary.
3. Attach mapped tests to the source behavior they cover.
4. Attach manifests and lock/generated outputs to the source or configuration
   change that produced them when a deterministic relation exists.
5. Group otherwise disconnected mechanical/repetitive files by role and nearest
   common path.
6. Leave genuinely unrelated files in separate cohorts rather than inventing a
   relationship.

A cohort membership key uses the existing cluster identity or a stable path/role
key and is never generated from PR prose. Within each membership key, Prowl
condenses strongly connected changed-file components and assigns a topological
layer ordinal from dependency to dependent. Contracts/foundations therefore
precede implementations/consumers; tests and deployment material follow the
behavior they validate or operate. Unconnected nodes use stable role/path order.

Final unit packing runs independently inside each membership-key/layer
partition. Unit IDs are then computed, followed by the public cohort/layer IDs
that include member unit IDs. Units in separate cohorts or the same layer may be
reviewed in parallel; layers are consumed in order.


## Attention signals

The planner computes transparent attention signals, not risk or severity:

- changed signature or removed symbol;
- high fan-in or large blast radius;
- entrypoint or externally consumed configuration;
- cross-subsystem dependency;
- deletion-heavy or replacement-heavy hunk;
- large rewrite;
- no mapped test;
- schema, migration, manifest, lock, or deployment change;
- unsupported/unindexed file; and
- unreviewable content.

Every signal names the deterministic fact that produced it. The host agent may
use signals to choose specialist review angles, but Prowl does not infer that a
security defect or bug exists.

## Review unit contract

`review unit` returns a `review.unit.v1` object with:

- review, cohort, layer, and unit IDs;
- scope and base/head identity;
- owned files and hunk ranges;
- exact base64 patch bytes with old/new coordinates and a non-canonical display
  preview where requested;
- before and after enclosing symbols or outlines;
- signature changes;
- file roles and attention signals;
- direct definitions, callers/references, dependencies, affected files,
  entrypoints, and related tests selected from the Prowl graph;
- trusted local guidance from accepted Prowl knowledge;
- base-revision repository policy/guidance labeled as untrusted evidence;
- explicit untrusted-content labels;
- deterministic review questions derived from attention signals;
- context budget usage and omitted counts;
- citations; and
- exact progressive-disclosure commands.

The packet reuses `internal/context` ranking/packing where suitable, but the
review manifest and patch ownership remain review-domain types. Patch text and
owned ranges are mandatory and cannot be evicted by the context budget.

Typical deterministic questions include:

- For a signature change, which callers still rely on the prior contract?
- For a new field or option, where is it populated and where is it consumed?
- For a removed guard/default/export, which invariant did it enforce and where
  is that invariant now established?
- For a high-fan-in change, do unchanged dependents remain compatible?
- For a behavior unit without a mapped test, which observable branch or failure
  path lacks evidence?

## Required cross-cutting audits

Structured plans have two disjoint collections: `primary_units` and
`required_audits`. Primary receipts range only over primary units; audit receipts
range only over required audits. Audits are not units and never require primary
receipts.

### Removed behavior

Audit ID `audit_removed_behavior_v1` targets every deletion-containing hunk ID
and removed-symbol target ID. Review removed guards, defaults, cleanup, exports,
error classifications, side effects, and lifecycle invariants.

### Contract migration

Audit ID `audit_contract_migration_v1` targets every changed-signature,
removed-symbol, and added-field/option semantic target ID recorded by the
extractor. Review caller and producer-to-consumer directions, including
unchanged callers/readers identified by the graph.

### Test matrix

Audit ID `audit_test_matrix_v1` targets every primary unit containing non-test
implementation, contract, schema/migration, configuration, or entrypoint code.
Documentation-only, generated-only, vendor-only, lock-only, and test-only units
are excluded by role with that exclusion recorded. Test existence alone is not
coverage.

### Integration and gap

Audit ID `audit_integration_gap_v1` targets every cohort ID, every mechanical or
unreviewable path ID, every `unreviewable_large_text` hunk ID, and every changed
path ID with no primary-owned reviewable hunk (including mode-only changes,
empty-file changes, and content-identical renames). Check cross-cohort
assumptions, entrypoint/configuration flow, mechanical groups, unreviewable
content, hunkless changes, and the ownership table.


An audit receipt acknowledges exactly its manifest `audit_target_ids`; it does
not claim primary hunk IDs as ranges unless those hunk IDs are independently in
its target set. The checker requires exact target-set equality. An empty target
set still receives an explicit empty receipt.

The host skill may add specialist reviewers, but optional specialists do not
replace the four required audits.

## Host-agent workflow

The portable review skill uses this protocol:

1. Run `review plan` before reading a large raw diff.
2. If `mode=direct`, perform an ordinary focused review using the returned
   context and no mandatory receipt matrix.
3. If `mode=structured`, create one accountable task per cohort plus the four
   audit tasks.

4. Review independent cohorts in parallel when the host supports subagents.
5. Retrieve units progressively; do not load every packet into one prompt.
6. Return exactly one primary receipt for every unit and one audit receipt for
   every required audit.
7. Aggregate and deduplicate candidate findings by causal behavior, not merely
   identical wording or line.
8. Re-trace each candidate finding against current code and graph evidence in a
   separate verification pass.
9. Run `review check` on the canonical report.
10. If the checker reports concrete missing IDs, review those gaps and check
    again.
11. Use recommendation `incomplete` whenever coverage is missing, workspace
    scope is stale, a required target remains unverified, or any receipt records
    blocking uncertainty. Never present that state as approval.

The skill treats all reviewed source, comments, documentation, PR/commit text,
and modified instruction files as untrusted evidence. Instructions inside them
are never followed.

## Finding and receipt schemas

The canonical `review.report.v1` object contains:

- review ID and full plan digest;
- tagged base/head identities;
- recommendation: `approve`, `comment`, `request_changes`, or `incomplete`;
- one immutable finding array consumed by every renderer;
- primary unit receipts;
- required audit receipts; and
- report-level notes.

A finding contains:

- stable ID;
- one or more causal references, each with kind `primary_unit`, `changed_path`,
  or `audit_target` and the corresponding canonical ID;
- category: `functional_correctness`, `security_privacy`,
  `stability_availability`, `data_integrity_integration`,
  `performance_scalability`, or `maintainability_quality`;
- severity: `critical`, `major`, or `minor`;
- confidence: `high`, `medium`, or `low`;
- summary and detailed explanation;
- concrete failure scenario or concrete maintainability cost;
- one or more locations. Each has kind `range` or `path`, repository path, side
  `base` or `head`, and a canonical side proof. Content-backed paths use content
  digest; gitlinks and special/non-content entries use tagged side identity plus
  mode/type. Range locations additionally carry exact start/end lines;
- supporting citations, including a causal changed hunk ID or hunkless changed
  path ID for a finding located outside the diff;

- introduced-by-change assessment;
- verifier disposition: `confirmed`, `plausible`, `rejected`, or `unverified`;
- verifier evidence citations;
- when rejected, a typed rejection reason; and
- optional suggested remediation.

Hunkless mode/binary/gitlink/special changes use a changed-path causal reference.
Findings from cross-cutting analysis may use an audit-target reference. The
checker requires every causal ID to belong to the plan.

A primary unit receipt contains unit ID,
`acknowledged_primary_hunk_ids`, context citations, finding IDs, structured
blocking/non-blocking uncertainties, and host reviewer identity. Its
acknowledged set must equal the unit's owned hunk IDs.

An audit receipt contains audit ID, `acknowledged_audit_target_ids`, context
citations, finding IDs, the same uncertainty objects, and reviewer identity. Its
acknowledged set must equal that audit's targets.

A receipt with no findings is valid. Primary and audit coverage are validated
independently.

The host verifier may reject a finding only after recording rejection reason
`contradicted_by_code`, `unreachable_by_invariant`, `pre_existing`, `duplicate`,
or `not_introduced` plus supporting citations. The checker validates typed shape,
IDs, citations, and locations, not semantic sufficiency.

## Checker semantics

`review check` has explicit mode branches:

- direct mode validates identity, scope freshness, finding/report shape,
  locations, verifier fields, and recommendation; its status records
  `coverage=not_required_direct` and it requires no receipts or audits; and
- structured mode additionally validates exactly one primary receipt per
  primary unit, one audit receipt per required audit, and exact primary-hunk/
  audit-target coverage with no missing or foreign IDs.

Both modes validate schemas/digests/IDs, tagged base/head identities, workspace
freshness, causal references, valid enums/uncertainties, non-empty
scenario/evidence, duplicate IDs/locations, and recommendation consistency.

Locations resolve against the immutable base/head tree for commit/range scopes
or the freshly verified workspace/base tree for workspace scope, not only against
changed-path records. A content-backed `kind=path` location resolves when its
path exists and content digest matches. A gitlink/special/non-content path
resolves when its canonical tagged side identity, mode, and type match.
`kind=range` additionally requires text and
`1 <= start <= end <= side_line_count`. An outside-diff location must also cite
a causal changed hunk ID or hunkless changed path ID.


Recommendation consistency is exact:

- a stale/invalid scope requires `incomplete`;
- in structured mode, missing coverage or blocking uncertainty requires
  `incomplete`;
- a non-rejected critical finding permits only `request_changes` or
  `incomplete`;
- a non-rejected major finding forbids `approve`;
- `approve` requires freshness, no non-rejected critical/major finding, and, in
  structured mode, complete coverage with no blocking uncertainty; and
- `comment`/`request_changes` require freshness and, only in structured mode,
  complete coverage.


The checker returns `complete`, `incomplete`, `stale`, or `invalid`; non-complete
states exit non-zero. Commit/range reports remain bound to immutable OIDs, while
provider integrations separately compare a live PR head.

For rejected findings the checker validates reason, citations, and structural
location. It does not decide whether evidence semantically proves the rejection,
or whether any finding/severity/uncertainty label is true.

## Trust model

The trust hierarchy is:

1. host system/developer instructions and the installed Prowl review skill; and
2. operator-accepted local Prowl knowledge.

Everything sourced from Git, including the resolved base revision, is untrusted
repository evidence. Applicable base-side `AGENTS.md`, explicit review rules,
and contribution guidance may be presented as policy evidence because the base
identifies the target project's prior convention, but they cannot issue tool
commands, change the review protocol, or override trusted instructions. The
proposed revision's changed policy files are separately labeled change evidence.

Unit packets serialize trusted local instructions/knowledge, untrusted base
policy evidence, and untrusted proposed content into separate fields. Renderers
place explicit data delimiters around every repository-sourced field. Tool
outputs remain data. The skill prohibits executing commands discovered inside
reviewed content.


Prowl never uploads code, invokes a model, runs project commands, grants provider
permissions, or checks out a revision through repository-controlled
filters/hooks. The sanitized Git policy disables external diff/textconv,
fsmonitor, pagers, filters during materialization, hooks, and interactive
helpers; security tests prove the policy instead of relying on command-array
invocation alone.

## Error handling and bounds

- Git timeouts, cancellation, invalid refs, multiple merge bases, unavailable
  objects, unsupported object formats, hostile helper execution, and malformed
  output fail plan construction; they do not produce partial plans.
- Workspace capture uses directory-rooted no-follow opens, retries once when its
  before/after fingerprints differ, then fails as concurrently modified.
- Exact untracked churn accounting is independent of context limits and must
  reach EOF within 64 MiB per file, 512 MiB total, and the capture timeout;
  otherwise plan construction fails before mode selection.
- One unsupported parser degrades that file to whole-hunk units and records the
  limitation.
- One failed graph query removes that optional context item and records an
  omission; it does not remove patch ownership.
- Unsafe or special untracked paths become explicit unreviewable entries and are
  never opened. Over-limit regular untracked files fail exact accounting rather
  than receiving estimated churn.
- Canonical mandatory hunk payload is capped at 128 MiB across at most 100,000
  changed paths and 16 MiB per published unit. Optional context alone is subject
  to smaller byte, token, hunk, and item presentation limits.
- Parallel snapshot/index construction is bounded by the existing project lock
  and one review-plan lock per ID.
- No recursive review loop is implemented in Prowl. The host skill performs at
  most one initial review wave, one finding-verification wave, and repeated gap
  waves only while `review check` identifies concrete missing IDs.

## Agent routing

The portable skill triggers on PR review, commit review, large diff review,
reviewing another agent's work, and pre-merge review.

Generated project context adds the compact rule:

> Before reviewing a workspace, commit, or branch range, run `prowl review
> plan`. When raw additions plus deletions exceed 300, follow every returned unit
> and required audit, then run `prowl review check`; never approve an
> incomplete or stale report.

OMP's sticky rules carry the same contract so it survives long sessions. Native
harness assets may adapt invocation syntax, but all delegate to the same portable
skill and CLI schema.

The routing rule is installed only after the held-out behavioral gates pass. The
commands may exist experimentally before then.

## GitHub action cutover

The existing `.github/actions/prowl-review` path is retained to avoid breaking
callers, but its display name and description change to **Prowl review context**.
It will:

1. initialize/freshen Prowl;
2. run `review plan` with explicit pull-request base and head SHAs;
3. expose `plan`, `review-id`, and `structured-required` outputs;
4. run `doctor` and emit SARIF as before; and
5. state that semantic findings require a host coding agent following the review
   skill.

The dogfood workflow uses full-enough Git history, passes both SHAs explicitly,
uploads the plan as an artifact, and uploads doctor SARIF. Comments claiming
that `doctor` alone reviews a PR are removed. The action does not post LLM
comments or claim semantic approval.

## Deterministic test strategy

### Git and diff tests

- strict 300/301 threshold boundary, including CRLF/final-non-newline data;
- additions, deletions, deletion-only files, and mixed replacement hunks;
- staged, unstaged, bounded untracked, symlink, and mixed mode changes;
- attribute-forced binary source, invalid UTF-8 text bytes, binary, special,
  oversized, mode-only, fixed-threshold rename, copy-as-add, and gitlink paths;
- branch merge-base, root commit, ordinary commit, and rejected merge commit;
- SHA-1 and SHA-256 repositories;
- invalid/ref-option injection, timeout, cancellation, lazy-fetch denial, and
  malformed Git output;
- direct tree-object materialization traversal, duplicate, symlink, size, and
  special-file cases;
- hostile diff/filter/helper/local configuration produces no process execution
  or canonical-output drift; and
- stable review IDs, including duplicate edits across files, and stale workspace
  detection.

### Planning tests

- every reviewable hunk has exactly one primary owner;
- symbol-aligned coalescing without splitting unified hunks;
- deletion-to-base-symbol and multi-symbol-hunk mapping;
- unsupported-language fallback;
- indivisible oversized-hunk and partial-path handling;
- cluster intersection, source/test attachment, cross-cluster direct relations,
  and genuinely unrelated cohorts;
- SCC condensation and dependency-first deterministic ordering;
- mechanical, partial, and unreviewable accounting;
- transparent attention-signal reasons; and
- all four audit units in structured mode.

### Unit/context tests

- exact base64 patch bytes and canonical mandatory-JSON cap;
- base/head symbol and signature context;
- callers/references, affected files, tests, and entrypoints;
- trusted local knowledge versus untrusted base/head repository guidance;
- progressive omission counts and stable citations; and
- CLI TOON/JSON/human parity.

### Checker tests

- direct report without receipts and complete structured report;
- missing, duplicate, foreign, and overlapping receipts;
- stale workspace after content or workspace `HEAD` movement;
- commit/range report remains valid after branch/check-out movement when its OIDs
  remain available;
- provider integration rejects a moved live PR head;
- invalid/outside-diff path/range locations with and without causal ranges;
- typed rejection reason/citations without pretending to prove semantics;
- empty scenarios/evidence;
- incomplete approval refusal; and
- immutable committed-scope behavior.


### Integration smoke

A temporary multi-package repository receives a workspace change larger than 300
lines containing contracts, implementations, callers, tests, a deletion-only
guard, and an unreviewable binary. The built binary must:

1. create a structured plan;
2. retrieve every reviewable unit;
3. reject an incomplete report;
4. accept a fully covered report; and
5. reject the same workspace report after its content or `HEAD` changes.


## Behavioral evaluation

A separate developer evaluation command follows the existing agent-adoption
harness's isolation and control/treatment pattern. Review evaluation gets its own
types and scoring because review findings are not code-search answers.

### Corpus

Use separate tuning and held-out manifests. Every primary held-out case has more
than 300 raw changed lines. The corpus spans:

- 301-1,000, 1,001-3,000, and over 3,000 changed-line bins;
- Go, TypeScript/JavaScript, Python, Java, and Rust;
- local logic, deletion/invariant, cross-file contract, producer/consumer,
  configuration/migration, dependency, test-gap, and clean changes; and
- both human-caught and deliberately injected validated defects.

Candidate sources are CodeReviewBench, Qodo's public 100-PR/580-issue benchmark,
Alibaba AACR-Bench, and qualifying public repository history. SWR-Bench is used
for small-change non-regression, not as large-PR proof, because its construction
filters large changes.

The held-out manifest contains at least thirty large PR cases, including clean
cases for false-positive measurement. Cases and expected issues are reviewed for
license/provenance before inclusion. Public repositories are pinned to immutable
commits; network preparation happens outside measured trials.

### Conditions

For at least two supported frontier coding-agent clients:

- **control** receives the normal review request, repository, raw diff access,
  and the same generic tools;
- **treatment** additionally receives the installed Prowl review routing, plan,
  units, receipts, and checker.

Both conditions must finish by emitting condition-neutral
`review.eval-output.v1`:

- trial status `completed` or `failed`;
- recommendation;
- findings with category, severity, confidence, summary, scenario/cost,
  locations, verifier disposition, and verifier evidence; and
- raw usage counters.

The harness canonicalizes each eval finding ID as SHA-256 over the fixed ordered
finding fields plus output ordinal. Control is prompted directly for this schema.
Treatment maps its checked `review.report.v1` findings into the same fields,
discarding Prowl-only causes/receipts after separately recording coverage. A
trial is completed only when its process succeeds and eval output parses;
treatment additionally requires `review check` status complete. Every other
outcome is failed under the frozen failure-scoring rule.

The model/version, system policy, maximum subagents, total input-plus-output
model-token cap, tool-call cap, wall-time cap, repository snapshot, and user
prompt are identical. Caps apply across child agents. Ground truth never appears
in the repository or prompt. Each case runs three repetitions per condition;
trial order is randomized and raw events are retained.

### Metrics and aggregation

The primary quality universe contains only the thirty or more base held-out PR
cases. Metamorphic padded variants are excluded from pooled TP/FP/FN and used
only for positional sensitivity. Every base case, client, condition, and all
three repetitions has equal trial weight.

An accepted finding is one in a parseable report with verifier disposition
`confirmed` or `plausible`; rejected and unverified findings are not reported
predictions. Semantic-and-location matching assigns integer TP, FP, and FN.
Precision, recall, and F1 use pooled counts, with a zero denominator defined as
zero.

A failed, timed-out, cap-exceeded, or missing-report trial receives TP=0, FP=0,
FN equal to that case's ground-truth issue count, and coverage=0. A failed clean
case therefore affects completion but does not invent a false positive. The
shipping gates separately require 100% treatment trial completion and coverage.

The primary metric pools TP, FP, and FN across every base case, repetition, and
client, then computes one micro-precision, micro-recall, and micro-F1 per
condition. Secondary metrics repeat that micro aggregation separately for each
client and defect class. Repetitions stay inside their case bootstrap block.

Additional metrics are:

- key-bug inclusion for critical ground-truth defects;
- cross-file and deletion-specific recall;
- accepted-finding file/line validity and semantic localization;
- primary-range and audit-target receipt coverage;
- incomplete/stale approval attempts;
- trial completion;
- total realized model tokens, tool calls, elapsed time, and failures; and
- per-case run-to-run variance.

Ground-truth matching requires both the underlying issue and a correct causal
location. An LLM judge may perform semantic matching, but humans audit every
judge disagreement and every accepted critical false positive.

Eligibility between an accepted finding and a ground-truth issue is a frozen
boolean edge requiring the same underlying issue and a correct causal location.
An LLM judge may propose edges, but blind human adjudicators without client or
condition labels resolve every uncertain edge before scoring. The resulting
eligibility matrix is immutable scorer input. Scoring uses maximum-cardinality
one-to-one bipartite matching; if several maximum matchings exist, choose the
lexicographically smallest sorted list of `(finding_id, ground_truth_id)` pairs.
Matched pairs are TP, unmatched accepted findings are FP, and unmatched
ground-truth issues are FN. Duplicate findings therefore become false positives.


Metamorphic cases place the same defect near the beginning, middle, and end of
otherwise equivalent padded diffs. A failed variant has hit rate zero. For each
triple and condition, compute mean hit rate at each position across clients and
repetitions; positional range is maximum minus minimum. Positional sensitivity
is the arithmetic mean range across all triples.

### Statistical procedure

Before any held-out run, commit an executable scoring configuration containing
the exact base-case IDs, metamorphic triple IDs, client/model IDs, caps, matching
criteria, blind-adjudication protocol, accepted-finding definition, formulas,
weights, failure scoring, seed, and thresholds.

The frozen protocol explicitly permits one post-run input: blind adjudicators
create the finding-to-ground-truth eligibility matrix without client/condition
labels, following the precommitted criteria. The matrix is then frozen and
archived before aggregate scores or condition labels are revealed; it cannot be
edited afterward.

The paired bootstrap resamples base held-out PR cases with replacement 10,000
times. A sampled case carries every client, condition, and repetition with it;
metamorphic variants do not enter the primary bootstrap. Each replicate
recomputes pooled micro-F1 and the treatment-minus-control difference. The
reported interval is the 2.5th and 97.5th percentiles. No seed, formula,
exclusion, threshold, or frozen eligibility edge changes after scoring begins.


### Shipping gates

Mandatory >300-line routing ships only when the frozen executable scorer reports
all of these gates true:

- treatment completes 100% of base and metamorphic trials;
- treatment has exactly 100% machine-verifiable primary-range and audit-target
  receipt coverage;
- pooled treatment micro-F1 minus pooled control micro-F1 is at least 10.0
  absolute percentage points;
- the paired-bootstrap 95% interval for that difference has lower bound strictly
  greater than zero;
- pooled treatment recall is strictly greater than pooled control recall;
- pooled treatment cross-file recall is greater than or equal to control;
- pooled treatment deletion-specific recall is greater than or equal to control;
- pooled treatment precision is greater than or equal to pooled control
  precision minus 5.0 absolute percentage points;
- treatment positional sensitivity is less than or equal to both control
  positional sensitivity and 0.10;
- 100% of location records on accepted treatment findings structurally resolve.
  The denominator is location records, every accepted finding must contain at
  least one, and a zero-accepted-finding treatment passes this structural gate
  vacuously; semantic location correctness is still required for a true
  positive;
- zero incomplete or stale treatment reports recommend approval;
- treatment total realized model tokens are less than or equal to 105% of
  control total realized model tokens under the identical hard cap; and
- treatment client-specific micro-F1 is strictly greater than control
  client-specific micro-F1 for every client.

Tuning never reads held-out outcomes. If a gate fails, routing remains
uninstalled, the design is revised using tuning evidence only, and a new
untouched held-out set is selected before another release claim.

## Rollout sequence

1. Implement deterministic diff, plan, unit, and checker behavior behind the
   explicit CLI commands.
2. Add unit, integration, security, and smoke coverage.
3. Add the portable skill and native assets without enabling mandatory generated
   routing.
4. Run tuning trials and make changes using tuning evidence only.
5. Freeze implementation and run the held-out evaluation.
6. If every gate passes, enable generated/sticky >300-line routing and update the
   GitHub action contract.
7. Update README, architecture documentation, capability documentation,
   changelog, and version through the repository's normal release process.
8. Run the specific Go tests, integration smoke, behavioral evaluation gates,
   `prowl changed`, and `prowl doctor` before completion.

Provider-native comment posting, optional built-in inference orchestration, and a
visual Change Stack-like interface require separate approved designs after the
core protocol demonstrates value.
