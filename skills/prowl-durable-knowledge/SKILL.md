---
name: prowl-durable-knowledge
description: Use right after you resolve something that cost real effort and will come up again: why a design is the way it is, a convention the code does not state, a trap that bit you, a question a teammate will ask twice, or an answer you had to reconstruct because a past session did not write it down. Reach for prowl knowledge to propose, review, and accept durable notes anchored to real code. This is for durable understanding, not transient task state; unfinished work in the current branch belongs in prowl wip, not here.
---

# Prowl durable knowledge

An agent's context window is thrown away at the end of a session, so whatever you
figured out about the project evaporates unless you write it down where the next
agent will find it. Prowl stores accepted decisions, concepts, and gotchas
as portable OKF Markdown, separate from the disposable index, and serves them
back through `search`/`context` so understanding compounds across agents instead
of being rediscovered every time.

## When to capture

Capture a note when you resolve something that cost real effort and will matter
again: why a design is the way it is, a convention that is not obvious from the
code, a trap that bit you, or the answer to a question the user or a teammate
will ask again.

## Workflow

1. First run only: `prowl knowledge init`.
2. Write the note as OKF Markdown (a decision, concept, claim, or playbook) and
   propose it: `prowl knowledge propose --file note.md --target decisions/<slug>.md`.
   The command prints a deterministic diff before anything is stored.
3. Accept it once reviewed: `prowl knowledge accept <proposal-id>`.
4. Check health anytime: `prowl knowledge list` and `prowl knowledge lint`.

Anchor claims to real code where you can; prowl flags a note as stale when the
code region it points at changes, so future readers know when to re-verify. Keep
notes short and evidence-backed; do not capture routine steps or anything the
code already states plainly.
