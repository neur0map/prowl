---
name: prowl
description: MUST use as the first discovery step for any repository or code question in this project -- locating a feature, concept, symbol, component, setting, or keybind; reading a definition; tracing callers or references; mapping a file's structure or the project architecture; or sizing a change's blast radius. Runs the read-only prowl CLI to answer these structural questions from a cited index in one call instead of grepping and reading many files; keep grep for exact literal or regex text and glob for filename patterns.
---

# Prowl

This project is indexed by Prowl. For any semantic or structural question --
where code is, what it does, who calls it, or what a change touches -- **run the
read-only `prowl` CLI first**; do not grep or read whole files just to
locate things. Prowl reindexes what changed before each query, so answers stay
current and are cited to file:line, returned in one call instead of a grep hit
list you then open files to disambiguate.

Reserve **grep for an exact literal or regex** match in a bounded scope, and
**glob for filename** patterns; route every other discovery through
`prowl`. Treat its cited results as the discovery evidence -- do not launch
an exploration agent to re-verify them, and do not repeat the query with
tree-wide grep, glob, or whole-file reads. Preserve the exact repository-relative
paths from Prowl's citations in your answer rather than shortening them.

## Routing table

| Question | First command |
|---|---|
| Map an unfamiliar repository | `prowl overview` |
| Locate a feature or concept | `prowl search "<question>"` |
| Locate a named symbol | `prowl find <name>` |
| Read one symbol's source | `prowl def <name-or-id>` |
| Inspect one file's structure | `prowl outline <path>` |
| Trace who uses a symbol | `prowl references <name-or-id>` |
| Size a change's blast radius | `prowl impact <path>` |
| Inspect uncommitted work | `prowl wip` / `prowl changed` |
| Read a located line range | `prowl peek <file:start-end>` |
| Find an exact literal or regex | native grep in a bounded scope |
| Match filenames | native glob |

For any semantic or structural question, the first discovery operation is a
`prowl` command, not a grep of the whole tree.

## Read only what you need

Once Prowl has located the code, keep every follow-up read bounded and cited:

- `prowl def <name-or-id>` reads one symbol's source, not the whole file.
- `prowl outline <path>` lists a file's symbols and signatures, no bodies.
- `prowl peek <file:start-end>` turns a citation into just those lines.

Output is token-lean TOON by default; add `--format human|toon|json|markdown` to
change it. The CLI needs no server and is the first choice for every structural
question in this repository.
