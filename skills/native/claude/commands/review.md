---
description: Review a workspace, pull request, commit, large diff, or agent-authored change through Prowl's bounded coverage protocol.
argument-hint: [[--base <ref> --head <ref> | --commit <ref>] [--structured]]
allowed-tools: Bash(prowl-agent:*), Read, Grep, Glob
---

Review the change selected by these arguments:

$ARGUMENTS

Before running anything, load and follow the bundled
`prowl:prowl-pr-review` skill (the plugin installs it under the `prowl:`
namespace, so use that exact name). It is the single source of truth for scope
routing, bounded review order, receipts, audits, verification, reporting, and
the final check gate. Do not restate, shortcut, or replace its protocol.
