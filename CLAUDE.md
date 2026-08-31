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
cmd/asz/                        CLI: sources · collect · index · show · verify
pkg/record/                     landed envelope — public, adapters produce these
internal/index/                 derived lookup structure the assembler resolves against
internal/storage/               landing zone, atomic writes, cursors, session/index state, locks
internal/verify/                contiguity and digest checks over landed data
internal/adapters/claudecode/   the claude-code-local adapter
internal/config/                YAML config
tests/adapter/claudecode/local/ end-to-end suite over a synthetic fixture corpus
```

`pkg/` is public because third-party adapters need it. Built-in adapters stay in `internal/`.

**Phase 1 (implemented):** pull collection from local Claude Code files, plus a derived index built
while landing. No configuration of Claude Code required, and it works on history that already exists.
**Phase 2 (designed, not implemented):** parse and assembly — see `design-notes/02`.
**Phase 3 (not designed):** export and preview. OTLP push belongs here too; it adds measurement
only, never structure.

## Build

```sh
make build          # -> ./bin/asz
make test           # whole suite, -race
make test-e2e       # only the end-to-end adapter tests, verbose
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
- **The index is derived and disposable.** Delete it and it rebuilds from landed files; a schema
  bump discards rather than migrates. Nothing downstream may treat it as authoritative.
- **The index stores roles, not runtime field names.** A runtime's vocabulary stops at its adapter,
  so a rename there cannot reach the model. `internal/index` and the model docs must stay free of
  `promptId`, `parentUuid`, `agentId` and their kin.
- **A record type is not what a record is.** A `user` record is usually a tool result, not a human —
  only ~7% are typed by a person. Check `origin` and the content blocks, never `type` alone.
