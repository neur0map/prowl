---
description: Answer a repository question from Prowl's cited index instead of grepping and reading many files.
argument-hint: [what you are looking for]
allowed-tools: Bash(prowl:*)
---

Answer this repository question by querying Prowl's index with the read-only
`prowl` CLI, not by grepping or reading whole files:

$ARGUMENTS

Before running anything, load and follow the bundled `prowl:code-search` skill
(the plugin installs it under the `prowl:` namespace, so use that exact name).
Its routing table is the single source of truth for which `prowl` command
answers which question, so choose the command from there -- do not work from a
table restated here.

Keep the boundary the skill defines: reserve native grep for an exact literal or
regex match and native glob for filename patterns; every semantic or structural
question is answered by a `prowl` command. Report each answer with its
file:line citation.
