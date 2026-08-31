# AgentSessionizer

Conversation-level observability for long-lived AI agents. It assembles fragmented agent
telemetry — transcripts, subagent streams, workflow journals — into one durable conversation
structure.

Two boundaries carry the design: **Conversation** is the durable ownership boundary and its
identity is supplied, never inferred; **ExecutionStream** is the ordered continuity boundary,
within which model-message continuity is evaluated.

Apache-2.0. Personal project, not an ASF project.

## Documentation

| Where | What |
| --- | --- |
| `docs/` | **Official docs.** Indexed by `docs/menu.yml`; SkyWalking layout (`docs/en/...`). |
| `docs/en/concepts-and-designs/unified-conversation-model.md` | The model. Runtime-agnostic — no Claude Code vocabulary belongs here. |
| `docs/en/adapters/claude-code.md` | How Claude Code maps onto the model, and what it cannot supply. |
| `design-notes/` | **Working notes, not documentation.** Measurements, corrections, open questions. Unpolished by intent. |

Every claim in the docs is backed by a measurement against a real corpus. When changing them,
keep that standard: state what was measured, on what sample, and say `unavailable` rather than
approximating.

## Structure

```
cmd/asz/                        CLI: sources, collect
pkg/record/                     landed envelope — public, adapters produce these
internal/config/                YAML config
internal/storage/               landing zone, atomic writes, cursors, session state
internal/adapters/claudecode/   the claude-code-local adapter
```

`pkg/` is public because third-party adapters need it. Built-in adapters stay in `internal/`.

**Phase 1 (implemented):** pull collection from local Claude Code files. No configuration of
Claude Code required, and it works on history that already exists.
**Phase 2 (not implemented):** OTLP push, which adds measurement only — no structure.

## Build

```sh
make build          # -> ./bin/asz
make test           # go test -race
make check          # vet, lint, license headers, dependency licenses, tests — what CI runs
make help           # all targets
```

Go 1.25+. One dependency: `gopkg.in/yaml.v3`.

Every source file carries the Apache-2.0 header; `make license-fix` inserts missing ones.
`license-eye` is pinned to v0.9.0 — v0.7.0 does not build under Go 1.26.

## Invariants worth knowing before changing collector code

- **Landed files are write-once.** temp → fsync → rename → `chmod 0444`. Never appended.
- **Payload bytes are the source bytes**, byte-for-byte. `json.RawMessage` both directions.
- **Land before committing the cursor.** At-least-once is intentional; the reverse loses data.
- **`next_seq` is monotonic across all streams in a session**, so a session is processed
  single-threaded and under a lock. The filesystem is the authority on the counter.
- **Never use `bufio.Scanner` on source records.** Real lines reach ~1 MB, past its 64 KB
  default, where it fails silently.
