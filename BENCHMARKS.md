# Benchmarks

Prowl's core claim is token efficiency: an agent gets a small, cited answer
instead of reading whole files. `prowl bench` measures that claim the
honest, reproducible way, with **zero external services** - no API keys, no
cloud vector database, no embeddings.

## What it measures

For each question, `bench` compares three costs, all in estimated tokens
(deterministic `(bytes+3)/4`, the same estimator Prowl uses everywhere):

1. **prowl** - the tokens in Prowl's cited context packet (bounded, compact mode).
2. **read-files** - the tokens of the files that packet cites, read whole. This
   is what an agent pays without Prowl: grep, then open the matching files.
3. **whole-repo** - the tokens of every indexed file. This is the "load the
   relevant directories / put the codebase in context" baseline that hosted
   vector-database tools bill for.

No competitor numbers are fabricated here. Tools like `claude-context` measure
the same token axis but require a cloud vector DB (Zilliz/Milvus) and an OpenAI
embedding key, so they cannot run in this harness; the point of comparison is
the token cost of an answer, which is framework-independent, plus the operational
cost Prowl removes (keys, cloud, embedding spend).

## Result (this repo, `prowl`)

Run on the Prowl codebase itself (363 files, ~383k tokens), `prowl bench`:

| question | prowl | read-files | reduction |
| --- | ---: | ---: | ---: |
| how is the project structured | 1,796 | 53,795 | 97% |
| where is the program entrypoint | 1,792 | 45,480 | 96% |
| how is configuration loaded | 1,792 | 63,148 | 97% |
| how are errors handled | 1,798 | 85,583 | 98% |
| how is data stored or indexed | 1,792 | 25,739 | 93% |
| how does search work | 1,795 | 64,542 | 97% |
| **totals (6 questions)** | **10,765** | **338,287** | **97%** |

- Reading the cited files costs **31x more** than Prowl's cited packets.
- Loading the whole repo for one question costs **~213x more** than one Prowl answer.

Numbers move with the repo and the index; the command reprints current figures.

## Reproduce

```bash
prowl bench                       # default question set, human table
prowl bench --json                # machine-readable report
prowl bench --questions q.txt     # one question per line
prowl bench "how does auth work"  # ad hoc questions
```

Every run is local and deterministic (full-text ranking; local vectors are added
only when AI is enabled with a local Ollama model). Nothing leaves the machine.
