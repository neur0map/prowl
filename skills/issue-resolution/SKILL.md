---
name: issue-resolution
description: Use when asked to work through open GitHub issues end to end -- review, investigate, fix, then comment and close -- or phrasings like "clear the open issues", "triage and fix the bugs in gh", or "resolve the tracker". Investigates each issue against the code with the read-only prowl-agent CLI, lands a real fix that builds and passes the repo's checks, and closes the issue with a short, human-toned note. Keep grep for exact literal text and gh for the GitHub API itself.
---

# Issue resolution

Turn open GitHub issues into landed fixes, one issue at a time. Read the issue,
locate the code it names with Prowl, confirm the bug is real and still present,
fix it at the source, prove the fix, then close the issue with a short human
note. Prowl is the investigation engine; `gh` is only the issue API.

Never fabricate a fix or close an issue you did not resolve. If an issue is
already fixed on the current line, verify that and close it explaining so; if it
is invalid or needs the reporter, say that instead of forcing a change.

## 1. Read the tracker

- `gh issue list --state open` lists what is open; `gh issue view <N>` reads one,
  including the reporter's steps and any attached logs or screenshots.
- Take the reporter's described symptoms as ground truth. Do not re-derive what
  they already observed; use it to aim the investigation.

## 2. Investigate with Prowl, not a grep sweep

Route every "where is this / what depends on it" question through the index:

| Question | First command |
|---|---|
| Locate the feature or subsystem named | `prowl-agent search "<question>"` |
| Locate a named symbol | `prowl-agent find <name>` |
| Read the offending code | `prowl-agent def <name-or-id>` |
| See a file's shape | `prowl-agent outline <path>` |
| Find every caller before you change a contract | `prowl-agent references <name-or-id>` |
| Size the blast radius of the fix | `prowl-agent impact <path>` |

Reserve grep for an exact literal (an error string, a config key) and glob for
filename patterns. When an issue quotes an error message, grep for that exact
string to find where it is emitted, then switch back to `find`/`def` to read it.

An error string that no longer exists in the tree usually means the issue was
fixed since the reporter's build. Confirm the current code path with
`prowl-agent def`/`references` before deciding the bug is gone.

## 3. Fix at the source

- Change the cause, not the symptom. Migrate every caller a contract change
  touches -- `prowl-agent references` before editing an exported symbol so no
  callsite is missed.
- Follow the repo's own conventions and its `AGENTS.md`/`CONTRIBUTING.md`. Do not
  invent a second pattern beside an existing one.
- Update the tests the changed behaviour breaks, and add one only where a
  plausible bug would fail it.

## 4. Prove it, then commit

- Build and run the specific test or scenario that covers the change; a passing
  targeted test is the proof, not the whole suite.
- `prowl-agent changed` maps your edits to what they could affect, so you check
  the right things before committing.
- Commit through the repo's hooks. Never bypass them (`--no-verify` is
  forbidden); match the project's commit-message format.

## 5. Close it like a human

- `gh issue comment <N> -b "<note>"` then `gh issue close <N>`.
- The closing note has four rules: **short** (one or two sentences), **not
  technical** (no diagnosis, no diff, no file or symbol names), **not descriptive
  of what the issue was** (do not narrate or summarise the bug back at them), and
  **human, not AI** (no "I've resolved…", no bullet lists, no hedging boilerplate
  or upbeat filler). Acknowledge the report, say it is handled, done.
- Good: "Sorted this out on our end, thanks for the clear writeup. Closing."
  Bad: "I have fixed the reconcile_zen reconciler so it no longer overwrites
  policies.json and removed the two extensions."
- For an already-fixed issue, tell the reporter it should be resolved in current
  builds and to reopen if it recurs -- same short, plain tone.

Ask before anything destructive or before deleting unrelated code you did not
write. When several issues are independent, resolve them in separate commits so
each fix stays revertible on its own.
