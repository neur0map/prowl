# Versioning and release channels

One number, bumped automatically, carried into every published binary.

## The version

`var version` in `cmd/prowl/main.go` is the single source of truth. Nothing
else stores a version, and no human edits it.

```
0 . 15 . 3
│    │    └── patch: one per push to a publishing branch (main or unstable)
│    └─────── minor: rolls over when the patch would reach 9
└──────────── major: manual, never automated
```

**Every push to `main` (stable) and to `unstable` (preview) advances the patch.**
The ninth bump rolls into the minor and resets the patch, so `0.15.8` is followed
by `0.16.0`. The major stays manual: automation never decides that a release is
breaking.

The bump is committed by CI as `chore: bump version to vX.Y.Z (automated)`, and
the workflow skips it by matching the bot **author**, not text in the message.
Matching text is a trap twice over: it also matches any commit that merely
mentions the marker, and on `main` it would skip a merge whose tip happened to be
a bump.

> **Never write GitHub's CI-skip token in a commit message, even in prose.**
> GitHub skips *every* workflow for a head commit containing it, with no way to
> opt out. A stable release was once lost to a bump commit carrying it, and then
> the very commit introducing this document was skipped for quoting it. The bump
> no longer uses it.

## The channels

| channel | fed by | release tag | who gets it |
|---|---|---|---|
| **stable** | pushes to `main` | `vX.Y.Z`, plus rolling `stable` | every install, by default |
| **preview** | pushes to `unstable` | rolling `preview` | opt-in only |

Both channels publish the same five targets: `linux-amd64`, `linux-arm64`,
`darwin-arm64`, `darwin-amd64`, `windows-amd64`, each with a `.sha256`.

- **Stable** is what `prowl update` downloads. Each release is permanent
  under its own `vX.Y.Z` tag, and the rolling `stable` tag always points at the
  newest. Only reviewed work reaches it, because it only moves when `main` moves.
- **Preview** carries every `unstable` commit as it lands, unreviewed. Opt in per
  machine:

  ```sh
  export PROWL_UPDATE_CHANNEL=preview
  prowl update
  ```

  Unset the variable and update again to return to stable.

The `nightly` tag is retired. It is still refreshed in step with stable so
binaries released before the channel split keep updating; it will be dropped once
those installs have rolled forward. Do not point anything new at it.

## What the updater compares

`prowl update` does not compare version strings. It compares the running
binary's embedded VCS revision against the head commit of its channel's branch
(`main` for stable, `unstable` for preview), then downloads the channel's asset
and verifies its SHA-256 before replacing the executable in place. The asset is
the build for the running platform (`prowl-<os>-<arch>`, `.exe` on Windows); a
platform with no published build is refused, never handed another platform's
binary. The version string is for humans and for the changelog; the commit is
what decides freshness.

This is why the release build checks out the branch tip rather than the commit
that triggered it: on a publishing branch the tip is the bump commit, and a
binary built from the pre-bump commit would report an available update forever.

## The pipeline

Bump, build and publish are one workflow (`.github/workflows/release.yml`) on
purpose. They cannot be split: a bump pushed with `GITHUB_TOKEN` does not trigger
another workflow run, so a tag- or push-triggered release chained after it would
never fire.

```
push to main     ──► resolve + bump ──► gate + build ×5 ──► publish `vX.Y.Z` + `stable` (+ `nightly`)
push to unstable ──► resolve + bump ──► gate + build ×5 ──► publish `preview`
pull request     ──► resolve        ──► gate + build ×5 ──► publish nothing
```

The `gate` job runs the same gofmt/vet/test checks as CI on the exact commit;
the publish jobs depend on it, so a red push can never become a release.

Every build injects the resolved version with
`-ldflags "-X main.version=vX.Y.Z"`, and then asserts that `prowl --version`
actually reports it. Before this was enforced, releases were built with
`git describe` and shipped reporting a commit SHA, so the bumped version never
reached a single user.

## Releasing

Nothing manual. Every push to `main` cuts the next stable version and publishes
it; every push to `unstable` does the same on the preview channel. (Landing on
`unstable` first and merging to `main` remains the safer path for unreviewed
work.) `workflow_dispatch` with an explicit `version` input exists for recovery
(republishing a version or cutting one by hand), and is not part of the normal
flow.
