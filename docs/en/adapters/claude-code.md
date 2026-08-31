# Claude Code Adapter

How the [Unified Conversation Model](../concepts-and-designs/unified-conversation-model.md) maps onto
Claude Code. Every mapping below is stated with the evidence that supports it and, where a concept
cannot be supplied, said to be unavailable rather than approximated.

**Verified against** Claude Code 2.1.220 – 2.1.251. No earlier version was available for measurement,
so nothing here is confirmed below 2.1.220.

## Collection

The adapter reads local files. It requires **no configuration, no environment variables, and no
cooperation from Claude Code** — and because it reads what is already on disk, it works on
conversation history that predates its installation.

```text
~/.claude/projects/<slugified-cwd>/<session-id>.jsonl                       main stream
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/agent-<agent-id>.meta.json
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/agent-<agent-id>.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/subagents/workflows/<wf-id>/journal.jsonl
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/wf_<run-id>.json
~/.claude/projects/<slugified-cwd>/<session-id>/workflows/scripts/<label>-wf_<run-id>.js
```

A workflow script carries its run id in the filename, which is what makes it collectable
despite living outside its session's directory. Split on the **last** `-wf_`: a run id itself
contains hyphens (`wf_afa31e47-f6c`), so a greedy match starts at the first occurrence and
swallows any later one.

OpenTelemetry is an optional second channel that adds measurement only — see
[OTLP](#otlp-optional-second-channel).

### Discovery is session-first

A directory under `projects/` is named after the working directory Claude Code was launched in, with
`/`, `.` and space all collapsed to `-`. That transformation is **not reversible** — the real path is
recoverable only from record content.

More importantly, **a session's files are not confined to one such directory**. Claude Code files a
workflow run's *script* under whatever working directory the agent had at the time. Measured on a
real corpus, 7 of 61 sessions span more than one directory for this reason, one of them across six.
The conversation data itself - main transcript, child streams, journals, manifests - stays together;
it is the script that travels.

Discovery therefore scans every directory for entries whose name is a session id — both `.jsonl`
files and directories — and **groups by session id across directories**. A directory-first walk
would attribute a session's scripts to a directory that holds nothing else of it.

A directory is recorded as belonging to a session only once it has yielded a collectable source.
Otherwise a directory containing nothing we collect could veto the whole session through an
exclude pattern — which is why filtering keys on the session's *primary* directory, the one
holding its main transcript.

Two further rules:

- `memory/` is a sibling directory inside a project directory and is **not** a session. Match session
  directories by identifier shape, not by "is a directory".
- **Pruning is real and not atomic.** Claude Code deletes transcripts — the great majority of session
  ids known to its prompt history have none. A session directory whose main transcript has been
  deleted while its child streams survive is a normal state, not corruption.

## Object mapping

| Model object | Claude Code | Notes |
| --- | --- | --- |
| **Conversation** | `sessionId` *(default)* | overridable when an application supplies an explicit id |
| **Segment** | derived | an activity window chosen by the assembler; not a runtime concept |
| **Session** | `sessionId` | 1:1 with Conversation under the default mapping |
| **ExecutionStream** `main` | `<session-id>.jsonl` | the only stream that compacts |
| **ExecutionStream** child | `agent-<agent-id>.jsonl` | shares the parent `sessionId` |
| **Context epoch** | split by `system/compact_boundary` | main stream only |
| **Talk** | derived from the input → output span | not a runtime concept |
| **Run** | derived | not a runtime concept |

### Conversation identity

`sessionId` is the default conversation identity because **nothing on disk reliably links two
sessions**. `bridgeSessionId` spans two session ids in only 1 of 20 observed cases and can be the
empty string; the snake_case `session_id` field is entrypoint-gated and its apparent cross-session
links trace to a single stale value. No heuristic over `cwd`, `gitBranch`, `slug` or title is sound.

`/clear` starts a new `sessionId` and a new file, and is detectable at the **head of the successor
file** as a fixed three-record chain: a root `user` record with `isMeta:true` carrying
`<local-command-caveat>`, then a `user` record whose content is `<command-name>/clear</command-name>`,
then `system/local_command`. The successor carries no pointer back to its predecessor.

Applications that consider several `/clear`-separated sessions to be one piece of work must supply an
explicit conversation id.

### Execution streams

A child agent **always shares the parent's `sessionId`** — measured across 2,697 subagent files with
zero exceptions and zero files carrying more than one session id. It never gets a session of its own.
The partition is clean: no sidechain records in main transcripts, no top-level `agentId` in main
transcripts.

| | main | child |
| --- | --- | --- |
| `sessionId` | S | **S — identical** |
| `isSidechain` | absent | `true` |
| top-level `agentId` | absent | `^a[0-9a-f]{16}$` |

Nesting stops at depth 2: of 2,686 agents, 2,654 are depth 1 and 31 are depth 2, each carrying
`parentAgentId`. No depth ≥ 3 exists.

Child streams are structurally simpler than main: exactly one root per file, no duplicate record
ids, **no compaction at all**, and no rewind forks. Their model context accumulates without
interruption for the life of the stream.

### Context epochs

`system/compact_boundary` is an explicit, machine-readable epoch boundary — the model never has to
infer one. It carries `compactMetadata` plus **`logicalParentUuid`**, a pointer to the last
pre-compaction message that appears on no other record type anywhere.

- The boundary's `uuid` is the `parentUuid` of the immediately following `isCompactSummary:true`
  record, without exception.
- The summary's timestamp is **always earlier** than its own boundary's, by 1 ms to 1,097 ms. Never
  order an epoch boundary by timestamp.
- `preservedMessages.allUuids` may contain ids that exist nowhere on disk — records held in context
  but never written. Both those entries and `logicalParentUuid` must be typed unresolved-capable.
- The preserved segment does not necessarily abut the boundary; resolve by id, never by proximity.

## Step mapping

| Model kind | Claude Code source | Quality |
| --- | --- | --- |
| `message.external` | `user` record, `origin.kind:"human"` | `exact_unique` |
| `message.assistant` | assistant `text` block | `exact_unique` |
| `message.synthetic` | `message.model == "<synthetic>"` | `exact_unique` |
| `context.injection` | `attachment` records, 32 types | `exact_unique` |
| `llm.call` | `(file, message.id)` group | `exact_unique` |
| `thinking` | assistant `thinking` block — signature only | `unavailable` (content) |
| `tool.call` | assistant `tool_use` block | `exact_unique` |
| `tool.result` | `tool_result` block + `toolUseResult` | `exact_unique` |
| `tool.execution` | — | `unavailable` in local logs |
| `agent.call` | `tool_use` named `Agent` / `Workflow` / Skill fork | `exact_unique` |
| `agent.launch_ack` | `toolUseResult.status:"async_launched"` | `exact_unique` |
| `agent.output` | final assistant output of the child stream | `exact_unique` |
| `runtime.notification` | `<task-notification>` block | `exact_unique` |
| `epoch.boundary` | `system/compact_boundary` | `exact_unique` |
| `epoch.summary` | following `isCompactSummary:true` record | `exact_unique` |
| `error.api` | `system/api_error` + `retryAttempt` | `exact_unique` |
| `control.interrupt` | `[Request interrupted by user]` | `exact_unique` |
| `control.permission` | denial strings, `permission-mode` records | `strong_inference` |
| `control.command` | `system/local_command` | `exact_unique` |
| `turn.duration` | `system/turn_duration` | `exact_unique` |

### Provider calls

**Group by `(file, message.id)` — never by `requestId`.** `message.id` is present on every assistant
record; `requestId` is absent on some genuine calls, and a `<synthetic>` companion record can reuse a
real call's `requestId`, which would splice fabricated content into a real provider call.

A single response is split across several JSONL lines, one content block each — mean 2.0, maximum 12.
Line order is authoritative; timestamps are not, and ties occur.

**Usage: take the last fragment. Never sum, and never assume fragments agree.** Main transcripts stamp
the final usage on every fragment; **subagent transcripts stamp streaming partials** (`output_tokens:
1, 2, 3 …`). Summing inflates output tokens by 2.14× on average and up to 10×. `output_tokens` is
monotone non-decreasing in line order with last == max, without exception.

About 7% of calls never receive a terminal fragment (0.02% main, 13.7% subagent). Their usage and
`stop_reason` are permanently unavailable, and a reader must not block waiting for one.

### Agent spawn — three mechanisms

No single field spans all three.

| Mechanism | Parent → child join |
| --- | --- |
| **Agent tool** | `meta.json.toolUseId` → the `Agent` `tool_use`; and the parent's `toolUseResult.agentId` |
| **Skill fork** | `toolUseResult.agentId` with `status:"forked"` — `meta.json` has no `toolUseId` |
| **Workflow** | the `Workflow` `tool_result`'s `toolUseResult.transcriptDir` and `runId`; plus `journal.jsonl` `{type:"started", agentId}` |

Asynchronous return is fully identified: `toolUseResult.status:"async_launched"` is the launch
acknowledgement, and completion arrives as a `<task-notification>` whose `<task-id>` **is** the
`agentId`.

Two cautions. The notification's `<tool-use-id>` is **not always resolvable in the file it lands in** —
nested children notify the main session while their spawning `tool_use` lives in an agent file, so
resolution must search the session's whole file set. And `<status>` (`completed | failed | killed |
stopped`) is **absent entirely** on a large class of notifications; parsing it unconditionally will
fail.

The workflow journal **leads the filesystem**: a `started` — even a `result` — can be journalled
before the child transcript exists. That is normal for a live session, not corruption.

## Reconstruction rules

Three rules, each required for correctness rather than efficiency.

**1. Deduplicate by `(file, uuid)` before anything else.** Record ids are not unique within a file:
duplicated pairs occur in a minority of main transcripts, always with multiplicity exactly 2. Keep the
first occurrence for position and merge fields the later copy adds. Before deduplication, tool joins
are ambiguous for a measurable fraction of results.

These duplicate blocks are not noise. Each is a pre-compaction re-emission of **exactly** the set of
records lying off the following boundary's ancestry — which independently establishes that
**context = chain ancestry ∪ off-chain siblings**, and distinguishes parallel-tool siblings (in
context) from abandoned branches (dropped).

**2. Assemble by `(message.id, line order)`, never by walking `parentUuid`.** The `parentUuid` graph is
a **DAG, not a chain**: parallel tool dispatch forks it, so an ancestor walk traverses one branch and
silently drops the other `tool_use` and its result.

> Ancestor-walk reconstruction produces a message list containing a `tool_use` with no matching
> `tool_result` — a body the Messages API would reject — on **39% of provider calls**. Line-order
> reconstruction is balanced in 2,622 of 2,623 tool-bearing files.

Every fork has exactly two children. Treat a fork as a topological hint, not a branch to choose.

**3. Always iterate the full `content` array.** A line usually carries one block but sometimes carries
several, mostly on `user` records — including `[image, text]`, where reading `content[0]` renders a
base64 image as the user's prompt.

## What this adapter cannot supply

| | Why |
| --- | --- |
| **Reasoning text** | Claude Code sends `thinking.display:"omitted"`, so the provider returns only a signature. Raw-body capture redacts it again. `unavailable` — not recoverable by any local mechanism. |
| **Serialized request** | system prompt, tool schemas and cache annotations are absent from transcripts. `input_manifest` is `truncated`, never `full`, with **inferred** message membership. |
| **Injected preamble** | the instruction block prepended to the first user message has no transcript record. Its *data* survives as `attachment` records; its rendered form does not. |
| **Per-call duration and cost** | not written to transcripts. Available via OTLP. |
| **`tool.execution`** | no local record; the call and result are observable, the execution is not. |
| **Retry attempt identity** | derivable only for failures that received an HTTP response; transport failures carry no request id. |

The injected preamble is a **fixed** cost — roughly 8 KB without a project instruction file, up to
~40 KB with one — so its share of a conversation falls as the conversation grows. It is not a
proportional loss.

Because the mapping from injected records to request blocks is **beta-gated and varies between client
versions**, and the active beta list is not recorded, a transcript-only reconstruction of the exact
request message list is not possible. Verifying model-context continuity against what was actually
sent requires captured provider bodies.

## OTLP (optional second channel)

Claude Code exports OpenTelemetry when `CLAUDE_CODE_ENABLE_TELEMETRY=1` and the relevant exporter
variables are set. It adds **measurement only**:

- per-call `duration_ms` and `cost_usd`, tool durations and error types
- `tool_decision` with a composite `source` (`hook:` / `rule:` / `mode:`)

It contributes **no structure**. Events carry no agent identity — `query_source` is normalised to
`main | subagent | auxiliary`, and the child-completion event carries no agent id at all. Agent
lineage exists only on spans, which require an additional beta flag; several span names present in
the binary are unreachable in shipped builds.

Content defaults are also restrictive: prompts and responses are redacted unless explicitly enabled,
content is truncated, and reasoning is always redacted.

## Local data hazards

Behaviours that will break a naive reader:

- **Session ids are not reliably lowercase.** Joins must be byte-exact and never case-folded.
- **Lines can be very large** — the longest measured is 979,632 bytes, and files reach 59 MB. A
  scanner with a 64 KB token limit fails silently.
- **`cwd` and `gitBranch` are per-record**, not session constants; one session showed 52 distinct
  `cwd` values. Only `sessionId`, `entrypoint` and `userType` are reliable per-file constants.
- **A large share of lines carry no record id at all** — metadata-only types that are not
  context-bearing. A parser assuming every line has one will fail.
- **Some files contain no conversation records at all**, holding only metadata types. Treating every
  `.jsonl` as a session produces empty conversations.
- **`tool_use_id` is not always prefixed `toolu_`** — server-side tools use a different prefix.
- **The corpus is live.** Files appear and grow during a scan; counts are snapshots.
- **`toolUseResult` is a main-stream enrichment.** On workflow children only the error-string form
  survives, and the key is *absent* rather than null.
