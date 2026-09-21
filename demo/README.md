# Demo

`prowl.tape` records a short terminal GIF of Prowl with the
[VHS](https://github.com/charmbracelet/vhs) tool: `init` (the ready card),
`overview`, `bench`, and `graph`.

## Render it locally

VHS needs `ttyd` and `ffmpeg` on your PATH.

```bash
go install github.com/charmbracelet/vhs@latest
cd /path/to/a/real/project    # prowl must be on PATH
vhs /path/to/prowl-agent/demo/prowl.tape
# writes demo/prowl.gif
```

Point it at a real repository (not an empty folder) so the language bars and the
benchmark have something to show.

## Render it in CI

The `demo` workflow (`.github/workflows/demo.yml`) builds Prowl, runs the
tape with `charmbracelet/vhs-action`, and uploads the GIF as a build artifact.
Trigger it from the Actions tab (workflow_dispatch); download the artifact and
drop it into the README's Demo section.
