---
name: prowl-change-safety
description: Use when about to edit, refactor, rename, or delete code, or before committing or opening a pull request, or when resuming a task and needing to see what was left unfinished. Reach for prowl impact before a change to see its blast radius, prowl changed and doctor after, and prowl wip to recover in-progress work. This answers what depends on the code you are touching; it does not run your tests, linters, or build, so keep using those for correctness.
---

# Prowl change safety

Before you change code, know what depends on it. After you change it, know what
you touched and whether the repo is still healthy. When you pick up a task, know
what the last session left unfinished. Prowl answers all three from the
local index in one command each, cited to file and line.

## Before editing a file

- `prowl impact <path>` gives the blast radius: how many files depend on it,
  the subsystems involved, and the direct importers. Use it to decide how careful
  to be and what to re-check.
- `prowl callers <path>` lists what includes, imports, execs, or binds to it.

## After editing

- `prowl changed` maps your git changes to the files they could affect, so
  you check the right things before committing.
- `prowl doctor` reports structural health (cycles, dangling references,
  fan-in risk) with a score. Add `--profile rice` for desktop and dotfile checks.

## Resuming a task

- `prowl wip` reports uncommitted work: touched files with their git status,
  the unfinished-work markers inside them (TODO, FIXME, and friends), and the
  blast radius of each indexed file. Run it first when picking up where a previous
  session stopped, instead of re-reading the tree to reconstruct state.

All of these re-index what changed before answering, so they never report stale
state.
