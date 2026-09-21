---
name: code-search
description: MUST use as the first discovery step for repository and code search -- locating a feature, concept, symbol, component, setting, or keybind; reading a definition; tracing callers or references; mapping a file's structure or the project architecture; or estimating the impact of a change. Runs the prowl CLI to answer these structural questions from a cited index instead of grepping and reading many files; keep grep for exact literal or regex text and glob for filename patterns.
---

# Code search

Answer repository questions by querying Prowl's index with the read-only
`prowl` CLI before grepping or reading whole files. Reserve **grep for an exact
literal or regex** match in a bounded scope, and **glob for filename** patterns;
route every semantic or structural question -- where code is, what it does, who
uses it, or what a change touches -- through `prowl`. Prowl
reindexes the files that changed before each query, so answers stay current and
are cited to file:line.

Treat Prowl's cited results as the discovery evidence. Do not launch an
exploration agent to verify them, and do not repeat the query with tree-wide
grep, glob, or whole-file reads. If more context is needed, stay in the index
with `def`, `outline`, `references`, or `peek`. In the final answer, preserve
the exact repository-relative paths from Prowl's citations rather than
shortening them.

## Routing table

| Question | First command |
|---|---|
| Map an unfamiliar repository | `prowl overview` |
| Locate a feature or concept | `prowl search "<question>"` |
| Locate a named symbol | `prowl find <name>` |
| Read a symbol definition | `prowl def <name-or-id>` |
| Inspect one file's structure | `prowl outline <path>` |
| Trace symbol use | `prowl references <name-or-id>` |
| Estimate change blast radius | `prowl impact <path>` |
| Resume or inspect current changes | `prowl wip` / `prowl changed` |
| Read a located bounded range | `prowl peek <file:start-end>` |
| Find an exact literal or regex | native grep in a bounded scope |
| Match filenames | native glob |

For any semantic or structural question, the first discovery operation is a
`prowl` command, not a grep of the whole tree.

## Read only what you need

Once Prowl has located the code, keep every follow-up read bounded and cited:

- `prowl def <name-or-id>` reads one symbol's source, not the whole file.
- `prowl outline <path>` lists a file's symbols and signatures, no bodies.
- `prowl peek <file:start-end>` turns a citation into just those lines.

Output is token-lean TOON by default; add `--format json` to parse it. Native
grep and glob stay correct when the task itself is an exact literal, regex, or
filename lookup, or when you are reading a file you have already located.
