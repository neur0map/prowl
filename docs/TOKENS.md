# Measuring token usage

The `prowl status` card shows an estimate of the tokens Prowl saved your
agent. This page explains how that number is computed and how to check it on your
own machine. Nothing here phones home; it is all local.

## What the number means

Every time your agent calls a prowl tool, prowl records two things:

- **answer bytes**: the size of the JSON it returned.
- **baseline bytes**: the combined size of the files that answer pointed at, i.e.
  the files an agent would otherwise have opened and read to find the same thing.

The saved estimate is:

```
saved_tokens = (baseline_bytes - answer_bytes) / 4 * 0.7
```

- `/ 4` converts bytes to tokens (the usual rough rule).
- `* 0.7` is a deliberate haircut. An agent might read only part of a file, some
  results overlap, and tokenizers vary, so prowl reports about 70% of the raw
  difference. The goal is to **under-count**, not to oversell.

It is an estimate, labeled as one. Treat it as a rough idea, not a guarantee.

## See your own numbers

```sh
prowl status          # the card, with per-project and combined savings
prowl status --json   # raw counters, including the savings block
```

The JSON `savings` block looks like:

```json
{ "queries": 142, "answer_tokens": 6300, "saved_tokens": 2100000 }
```

`queries` and `answer_tokens` are measured exactly; `saved_tokens` is the
conservative estimate above.

## Check it yourself, by hand

Pick a project and a word that names a real feature in it (a widget, a script, a
setting). The example uses `battery`; substitute your own.

```sh
cd ~/your-project
kw=battery
```

**Without prowl**, an agent searches and then reads the matches. Approximate that
cost (tokens are bytes / 4):

```sh
# what the search alone dumps into context:
rg -n "$kw" | wc -c

# what it costs to actually open every file that matched:
rg -l "$kw" | xargs wc -c | tail -1
```

(No ripgrep? `grep -rn "$kw" .` and `grep -rl "$kw" .` work the same way.)

**With prowl**, the agent asks one tool (`find_symbol`, `similar_code`, ...) and
reads a small, ranked answer instead. After it has run a few queries, compare:

```sh
prowl status --json | python3 -c 'import sys,json; print(json.load(sys.stdin)["savings"])'
```

You will usually see the answer measured in a few kilobytes where the
read-the-files path is hundreds of kilobytes to megabytes. Your repo, your
question, and your editor will move these numbers, so measure on your own setup.

## Bigger test subjects

If you want to try it on large, real configs rather than your own, these are good
public ones to index and poke at:

- https://github.com/neur0map/ryoku-arch
- https://github.com/end-4/dots-hyprland
- https://github.com/noctalia-dev/noctalia-shell

Clone one, run `prowl init`, point your agent at it, and watch the savings
add up across projects in `prowl status`.
