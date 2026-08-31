# Design Plan 02 — Parse and Assembly

**Status:** draft for discussion. Not implemented.
**Scope:** turning landed records into a conversation structure. Collection is Plan 01 and is
implemented; export and preview are Plan 03 and are not designed.

Every rule below is forced by a measurement in Plan 01. Where a rule looks arbitrary, the evidence
that forced it is cited, because several of them are the opposite of the obvious choice.

---

## 0. What parse consumes

Parse reads the **index**, not the payloads.

```
365,554 index entries    describing 1.0 GB of landed payload
47 MB total index        ~12x smaller
16 MB / 4.5 ms           largest single session, load + rebuild every lookup map
```

Structure resolution needs identifiers, not content: deduplication needs record ids, provider-call
grouping needs message ids, tool joins need tool-use ids, spawn joins need agent and run ids. None
of it reads message text. Payloads are read only in the final materialisation step, and only for
records that appear in the output.

The index is derived and disposable; deleting it rebuilds from landed files.

---

## 1. The pipeline

Eight stages. The order is forced: each depends on the one before, and two of them are wrong in the
obvious formulation.

### 1.1 Deduplicate by `(file, uuid)`, first wins

**Nothing downstream is correct before this.** Record ids are not unique within a source file: a
resume replays an earlier contiguous block. 1,530 duplicated pairs across 10 main transcripts,
**multiplicity exactly 2, never 3**, so the pass terminates.

First occurrence wins. Keeping the last relocates up to 415 records from their true position to the
resume point and scrambles line-order reconstruction.

Before dedup, tool joins are ambiguous for 1.4% of results — 882 of 64,722.

> The index already applies this: `byUUID` keeps the first occurrence.

### 1.2 Partition streams

`main` and one stream per `agent-id`, already separated by the landing layout. A child stream
**always shares the parent's session** — verified across 2,697 subagent files, zero exceptions.

Nesting stops at depth 2 (2,654 at depth 1, 31 at depth 2, none deeper).

### 1.3 Assemble provider calls by `(file, message.id)` in **line order**

**Never by walking `parentUuid` forward.** The graph is a DAG, not a chain: parallel tool dispatch
forks it, so a forward walk traverses one branch and silently drops the other `tool_use` and its
result.

The ban is on the **direction**, not on the edge. Walking *backward* - from a record to its nearest
ancestor carrying a given field - is single-valued and correct, because a record has exactly one
containment parent. Stage 1.7 depends on that. Only forward traversal is ambiguous.

> Ancestor-walk reconstruction yields a message list containing a `tool_use` with no matching
> `tool_result` — a body the Messages API would reject — on **39% of provider calls**. Line-order
> reconstruction is balanced in 2,622 of 2,623 tool-bearing files.

Group by `message.id`, **not** `requestId`: a `<synthetic>` companion record can reuse a real call's
`requestId`, splicing fabricated content into a genuine call.

Synthetic filter: `message.model == "<synthetic>"`. Not `requestId == null` — 24 of 97 synthetics
carry a non-null `requestId`, and 2 genuine calls have none.

**Usage: take the last fragment.** Main transcripts stamp the final usage on every fragment;
subagent transcripts stamp streaming partials (`output_tokens: 1, 2, 3 …`). Summing inflates output
tokens by 2.14x mean, 10x max. `output_tokens` is monotone non-decreasing with last == max in
56,524/56,524 groups.

About 7% of calls never receive a terminal fragment (0.02% main, 13.7% subagent); their usage and
`stop_reason` are permanently unavailable and must not be waited for.

Mean 2.37 fragments per call on the sampled session — treating records as calls would inflate every
count by that factor.

### 1.4 Join tools

`tool_use_id` → the index's `byTool`. Measured 1,211 uses and 1,211 results in the sampled session;
corpus-wide every `(file, tool_use_id)` maps to exactly one deduped result.

`tool_use_id` is not always prefixed `toolu_` — server-side tools use `srvtoolu_`.

A record can carry several tool blocks, which is why blocks are indexed separately from entries;
folding them in would drop all but one.

`tool.execution` has no local record and is reported `unavailable`. The POC preview renders a
three-phase call → execution → result; only two of those exist in Plan 1 data.

### 1.5 Join spawns — three mechanisms

No single field spans all three, and 70% of children carry no parent pointer in their own sidecar.

| Mechanism | Join |
| --- | --- |
| **Agent tool** | `meta.json.toolUseId` → the `Agent` `tool_use`; and the parent's `toolUseResult.agentId` (redundant, so cross-checkable) |
| **Skill fork** | parent's `toolUseResult.agentId` with `status:"forked"` — the sidecar has no `toolUseId` |
| **Workflow** | `Workflow` result's `toolUseResult.transcriptDir` + `runId`; plus `journal.jsonl` `started` records |

Async return: `status:"async_launched"` is the **launch acknowledgement, not the result**.
Completion arrives as `<task-notification>` whose `<task-id>` is the agentId.

Two cautions. The notification's `<tool-use-id>` is **not always resolvable in the file it lands in**
— nested children notify the main session while their spawning `tool_use` lives in an agent file —
so resolution must search the whole session. And `<status>` is **absent entirely** on a large class
of notifications; parsing it unconditionally will fail.

`agent.output` belongs to the **child** stream. The parent gets a compact boundary that references
it, never absorbs it — this is what stops a rendered conversation duplicating every subagent's work.

### 1.6 Cut context epochs

`system/compact_boundary` carries **`logicalParentUuid`**, an explicit pointer to the last
pre-compaction message, present on 26/26 boundaries and on no other record type anywhere. Epochs are
never inferred.

- boundary `uuid` == the `parentUuid` of the immediately following `isCompactSummary:true` record
- the summary's timestamp is **always earlier** than its boundary's, by 1–1,097 ms — never order an
  epoch by timestamp
- `preservedMessages.allUuids` may name records that exist nowhere on disk; both those and
  `logicalParentUuid` must be typed unresolved-capable
- child streams never compact — every child stream is exactly one epoch

### 1.7 Build Talks and Runs

This is the stage that produces the conversation a person reads, and the one with the least
runtime support: neither Talk nor Run is a Claude Code concept. Both are derived, so the boundary
rule has to be stated exactly.

**Two runtime signals map onto one model concept. Conflating them is the trap.**

`promptId` and `origin` are Claude Code's own undocumented fields. Neither is a model concept and
neither may appear outside this adapter; the model has Talk, and nothing below it.

| Side | | Meaning | Sampled session |
| --- | --- | --- | --- |
| runtime | `promptId` | one *prompt cycle* | 65 |
| runtime | `origin.kind` | what triggered that cycle: `human` or `task-notification` | 36 / 29 |
| **model** | **Talk** | a human trigger plus the notification cycles it caused | **36** |

The identity holds corpus-wide:

```text
distinct promptId  ==  count(origin.kind=="human")  +  count(origin.kind=="task-notification")
```

Exact in 10 of 12 sampled sessions. Both exceptions are explained: the session with 704 duplicate
uuids has 17 *fewer* promptIds because replayed copies share one, and one session has 2 more from
promptIds on records carrying no `origin`.

**A Talk starts only on `origin.kind == "human"`.** A background agent finishing and the parent
resuming is mechanically a new prompt cycle, but the human said nothing - it is the same interaction
continuing. Splitting on `promptId` would shatter one conversation into 65 fragments where a reader
sees 36. This is precisely the
`agent.call -> launch_ack -> child stream -> agent.output -> runtime.notification -> parent resumes`
chain of §1.5, and it lives INSIDE one Talk.

**Membership is resolved by walking backward, never by line proximity.**

`promptId` appears on `user` records only - assistant records carry none, verified across 115,184.
But every assistant record reaches one through `parentUuid`:

```text
assistant records tested          400
reached a promptId via parentUuid 400      (100%)
hops needed                       min 1, median 2, max 7
```

So a record's Talk is found by walking back to the nearest ancestor carrying a cycle id, then reading
that record's trigger kind. (The index exposes these as `Cycle` and the record's kind; the runtime
field names stop at the adapter.) Line-order proximity would agree in the simple case and diverge exactly
where it matters - a forked chain, a replayed block, an interrupt that re-parents to an earlier uuid
- attributing records to the wrong turn.

**A child stream is exactly one Talk**: the delegated prompt in, the final output out, no human
involved. Child streams never compact, so each is also exactly one epoch.

**Human turns that exist only as attachments must be recovered.** 614 corpus-wide are
`queued_command` records with no `.message` twin; a reader walking `.message` alone loses real human
input. They must be filtered on `commandMode == "prompt"` together with `origin.kind == "human"` -
without both, `task-notification` blobs get spliced in as things the user typed (24 of them in the
sampled session against 11 genuine prompts).

**Ownership rules** from the model: `context.injection` is first-class, `message.synthetic` is never
counted as agent output, `agent.launch_ack` is not a result, and `agent.output` is owned by the
child stream and only referenced by the parent.

### 1.8 Close segments — candidate, then gates

A ~10-minute idle gap creates a **close candidate**, not a commit. **~32% of gaps over 10 minutes
are mid-turn** (long tool calls, API retries), so a naive idle rule mis-splits roughly a third of
the time.

Gates before committing:

| Gate | Why |
| --- | --- |
| `activity_boundary` | the idle gap is real |
| `no_crossing_open_operation` | **no unfinished tool call or in-flight subagent.** Without this, a `tool_use` could land in a committed segment while its result arrives in the next — and committed segments are never rewritten |
| `lateness_watermark` | no late evidence still expected |
| `conversation_identity` | the conversation id is resolved |

The second gate is load-bearing rather than defensive: it is what makes "committed segments are
immutable" safe to promise.

---

## 2. Output structure

**The open segment is append-only; a committed segment is a single immutable file.**

The naive shape - rewrite the open segment's whole snapshot every round - fights the grain of
everything beneath it. Landed files append, cursors advance, the index appends. Rewriting a 15 MB
structure every five seconds to add a handful of nodes is the one place the design would stop being
incremental.

But pure append does not work either, because **assembly revises what it already emitted**. A `tool`
node lands with `result: unavailable` and gains its result next round. A provider call gains
fragments. An `agent.call` gains its child stream once the child transcript appears - and the
workflow journal names children before their transcripts exist, so this is routine, not an edge.

So the open segment is an append-only log of node *revisions*, folded by id on read:

```text
conversation/
  SG1/snapshot.jsonl          committed: folded once, then immutable
  SG2/delta-000004.jsonl      open: one append per round
     /delta-000007.jsonl
     /delta-000009.jsonl
  conversation.state          assembled_seq · open segment · revision
```

A reader folds deltas in sequence order, last write wins per node id. Committing a segment folds its
deltas into one `snapshot.jsonl` - which makes **segment commit the compaction point**, rather than a
separate mechanism.

**Every delta is write-once and read-only**, on the same terms as a landed file: temp name, fsync,
rename, `chmod 0444`. That is what makes a round's output a unit rather than a stage - it can be
digested, archived, or **shipped to a server the instant it is written**, without waiting for the
segment to close or risking that it changes underneath a reader.

Three properties follow, and they are the reason to prefer this over rewriting:

- **A round produces exactly one new immutable artifact.** Nothing already on disk is touched, so a
  reader mid-fold is never racing a writer.
- **Deltas are independently shippable and idempotent.** Each carries its own header, digest and
  counts, so a receiver can accept them in any order and fold by sequence. Re-sending one is a
  no-op.
- **A crash cannot corrupt prior output.** The worst case is a missing final delta, which the next
  round re-derives, because `assembled_seq` still points before it.

One thing shipping does not solve, and Plan 03 must: a delta references content by
`{"seq":…,"row":…}` into landed files. A receiver holding only deltas can render the structure but
not the text. Either the landed files travel too, or the export inlines content for the nodes it
describes - which is an export contract decision, not a storage one.

Each file is framed JSONL, and a delta carries only what changed:

```jsonl
{"t":"header","schema":"asb/1","conversation":"92a5d58e-…","segment":"SG2","revision":4,"base":"c41e…"}
{"t":"node","id":"main/t8","kind":"talk","stream":"main","epoch":"e0"}
{"t":"node","id":"main/t8/r1/c3","kind":"llm.call","parent":"main/t8/r1",
 "ref":{"seq":12,"row":340},"fragments":[{"seq":12,"row":340},{"seq":12,"row":341}],
 "input":{"base":"msg_011CeUH…","appended":[{"seq":12,"row":338}],
          "state":"truncated","membership":"inferred"}}
{"t":"relation","type":"spawns","from":"main/t8/r1/c3#toolu_01YYoton…","to":"stream:a4646a5c9cb88661d",
 "quality":"exact_unique","via":"toolUseResult.agentId","evidence":[{"seq":12,"row":345}]}
{"t":"unresolved","kind":"task_notification_tool_use","ref":"toolu_…","state":"transient"}
{"t":"commit","digest":"<sha256 of lines 1..n-1>","manifest":"<ledger digest>",
 "counts":{"nodes":1284,"relations":96,"unresolved":3}}
```

A node line in a delta **supersedes** any earlier line with the same `id`. Relations and unresolved
entries are keyed the same way, so a resolved reference is superseded rather than duplicated.

**The invariant that keeps this honest:** folding a segment's deltas must equal a full rebuild of
that segment from the index. Rebuilding is still the reference implementation - it is simple and
obviously correct - and the fold is checked against it. Where they disagree the fold is wrong, and
the disagreement is a test failure rather than a silently divergent snapshot.

**Content is referenced, never inlined.** `{"seq":12,"row":340}` locates the landed record. Storing
each call's input would multiply storage by conversation depth — roughly 220 MB for a session whose
actual content is 10 MB.

**Input manifests are deltas.** `base` + `appended`, which is exactly the continuity invariant, so
the representation *is* the claim. A full manifest appears only where the chain genuinely breaks:
compaction, fork, stream start. Those bases coincide with epoch boundaries.

**Containment is single-parent; causality is a sparse typed relation** carrying `via` and `quality`,
so an exact id match is distinguishable from an inference.

---

## 3. Continuous build

**Committed segments are immutable and never re-assembled. The open segment is rebuilt from scratch
each round.**

Rebuilding rather than mutating is what preserves the property that makes this testable: assembling
the same input range twice produces byte-identical output. Cost is not the argument — the open
segment resolves against an 8 MB index in milliseconds.

Rounds and segments are independent axes. Many rounds fold into one segment while work continues;
a segment closes only on an idle gap plus gates.

Watermark chain, each stage trailing the last:

```
assembled_seq  <=  indexed_seq  <=  next_seq - 1
```

Two watermarks are needed, not one: `assembled_seq` answers "is there work", `open_start_seq` is the
rebuild floor for the open segment.

**A per-stream uuid index must persist across commits.** A resume replays records whose first
occurrence may sit in an already-committed segment; without it, a replayed block lands as a
duplicate node.

**Verifiable invariant:** revision N+1 should be a prefix-extension of revision N. Where it
legitimately is not — a replay reordering after dedup, a compaction re-rooting an epoch — that is
exactly the interesting case, so the check reports rather than fails.

---

## 4. The unresolved catalog

Recorded as data, with two disciplines that make it useful rather than noisy.

**Transient vs permanent**, because both genuinely occur:

| Reference | Kind |
| --- | --- |
| journal `started` names an agent whose transcript does not exist yet | transient — the journal leads the filesystem |
| `<task-notification>` `<tool-use-id>` absent from the file it landed in | transient — resolves elsewhere in the session |
| `tool_use` with no `tool_result` | transient unless the session ended |
| `allUuids` entry existing nowhere on disk | permanent — held in context, never written |
| `logicalParentUuid` unresolvable | permanent |
| source in `source_gone` | permanent — pruned before collection |

**A denominator.** "3 unresolved" means nothing; "3 of 8,045 tool_use ids" means something.

A transient reference that stops resolving after N revisions is promoted to permanent.

---

## 5. Open questions

1. **Does a compaction boundary force a segment close?** I lean no — epoch is model-context
   lifetime, segment is a commit window; conflating them makes segments hostage to Claude Code's
   compaction timing.
2. **Exact idle threshold and gate parameters**, given ~32% of >10-minute gaps are mid-turn.
3. **Where a session's conversation id comes from** when an application supplies one, and how it is
   recorded so cold reactivation preserves it.
4. **Whether `conversation/` output is Plan 02's own format or v0.8's ASB.** I lean on keeping it
   internal and treating ASB as a Plan 03 export, so the storage format and export contract are not
   coupled before either is proven.
