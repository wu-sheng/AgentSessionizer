# Design Plan 02 — Parse and Assembly

**Status:** the round chain (§2) is implemented in `pkg/asb`; the pipeline (§1) is not.
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

## 2. Output structure — the round chain

Implemented in [`pkg/asb`](../pkg/asb/). The tests in `pkg/asb/asb_test.go` are the specification;
what follows is why it has this shape.

**A round is an append-only immutable delta, and the conversation is the fold of every round.**

```text
state₀ = empty
stateₙ = apply(stateₙ₋₁, roundₙ)
```

The naive shape — rewrite the whole structure every round — fights the grain of everything beneath
it. Landed files append, cursors advance, the index appends. Rewriting a 15 MB structure every five
seconds to add a handful of nodes is the one place the design would stop being incremental.

Pure append does not work either, because **assembly revises what it already emitted**. A `tool`
node lands with `result: unavailable` and gains its result later. A provider call gains fragments.
An `agent.call` gains its child stream once the child transcript appears — and the workflow journal
names children before their transcripts exist, so this is routine, not an edge.

The resolution is that a revision is a *new line in a later round*, never an edit to an earlier one.
That is what lets a round be digested, archived or shipped the instant it is written.

### 2.1 Layout

```text
data/_conversations/<conversation-id>/
  conversation.state                       mutable head pointer
  rounds/r000001-<digest12>.asb.jsonl      immutable, 0444
  rounds/r000002-<digest12>.asb.jsonl
```

The split is deliberate. **Rounds carry nothing mutable and nothing temporal**; `conversation.state`
carries the head round, its digest, how far landed evidence has been consumed, and when. Rounds are
linked by digest, not by filename: round N names the digest of round N−1, so a holder of round 3
that has not seen round 2 can store it but cannot apply it.

### 2.2 Frames

```jsonl
{"t":"header","schema":"asb/1","conversation":"C1","session":"S1","round":2,"previous":"<r1 digest>",
 "from_seq":46,"through_seq":57,"input_digest":"<chained landed digest>","parser":"v1","policy":"v1"}
{"t":"node","id":"main/t8","revision":2,"kind":"talk","stream":"main"}
{"t":"node","id":"main/t8/r1/c3","revision":2,"kind":"llm.call","parent":"main/t8/r1",
 "ref":{"seq":12,"row":340},"refs":[{"seq":12,"row":340},{"seq":12,"row":341}]}
{"t":"relation","id":"rel/spawns/main-t8-r1-c3/stream-a4646a5c9","revision":2,"type":"spawns",
 "from":"main/t8/r1/c3","to":"stream/a4646a5c9","quality":"exact_unique","via":"toolUseResult.agentId",
 "evidence":[{"seq":12,"row":345}]}
{"t":"unresolved","id":"unres/tool_result/toolu_01Y","revision":2,"kind":"tool_result",
 "ref":"tool/toolu_01Y","state":"open"}
{"t":"commit","digest":"<sha256 of lines 1..n-1>","counts":{"nodes":1284,"relations":96,"unresolved":3}}
```

The commit digest covers every line before it, so a round verifies itself: recompute over lines
1..n−1 and compare. It is also the value the next round names as its `previous`.

### 2.3 The four rules

**1. Entity ids come from stable evidence, never from position.** A node keyed by "the 14th record in
the stream" gets a different key the moment an earlier source is backfilled — and a changed key
cannot supersede its own earlier revision, so the fold would hold both, one stale and one current,
with no way to tell them apart. Ids are built from the runtime's own identity for a thing, scoped by
session and stream; where the runtime supplies no identity, from the landed location, which is
itself immutable once written.

**2. Last writer wins, per id, whole-entity.** A later revision replaces the earlier one entirely.
Not field-by-field: a partial merge cannot express "this attribute is now absent", so a correction
that removes something could never be recorded.

**3. Absence means unchanged.** An entity missing from round 7 is exactly the entity round 4
published. Removal is therefore explicit — a revision with `tombstone` set — and never inferred from
silence. Likewise an unresolved entry that gets resolved is *superseded* with `state:"resolved"`, not
deleted: absence would be indistinguishable from "never noticed".

**4. Order is chain order, not timestamp order.** Rounds fold in round order, entities within a round
in file order. Timestamps come from the observed runtime and can go backwards across sources; the
chain cannot.

### 2.4 Determinism, and what it forced

The property worth protecting is: *same previous digest + same landed range + same parser and policy
versions ⇒ same round bytes and same digest*. Without it a chain can only be trusted, never
re-derived. Three things in the earlier draft broke it, and each had to change:

- **`revision` was a per-entity counter.** Deriving it required replaying the chain, and two
  producers reaching the same state by different paths would disagree. It is now **the round number
  that produced the entity** — readable straight off the header, and identical for any producer of
  that round.
- **The header carried a timestamp.** Wall-clock time cannot be reproduced, so it would put every
  re-derivation in disagreement with the original. Time moved to `conversation.state`, outside every
  digest.
- **`input_digest` was described as a digest over all evidence read so far.** Recomputing it would
  make each round cost proportional to the whole conversation — a long-running one would slow down
  without bound, which is the exact property an incremental design exists to avoid. It is now
  chained: `H(previousₙ₋₁ ‖ sorted digests of what round n consumed)`, proportional to what is new
  and still binding every round transitively to all evidence before it. Inputs are sorted because
  directory iteration order is not stable.

### 2.5 What verification checks

`Chain.Verify` walks the chain and checks four things, each catching a failure the others miss:

| Check | Catches |
| --- | --- |
| rounds are 1..N with no gap | a missing round — its successors are unverifiable without it |
| each round names its predecessor's digest | a fork, or an edited round |
| each round's digest matches its bytes | tampering or truncation (`Read` does this) |
| `from_seqₙ == through_seqₙ₋₁ + 1` | **skipped landed evidence — no digest reveals this** |

The last is the one worth stating separately. Digests prove the rounds are the rounds; only the
sequence check proves they read everything.

### 2.6 What shipping does not solve

A round references content by `{"seq":…,"row":…}` into landed files. A receiver holding only rounds
can render the structure but not the text. Either the landed files travel too, or the export inlines
content for the nodes it describes — an export contract decision, deferred to Plan 03.

**Content is referenced, never inlined**, for the same reason: storing each call's input would
multiply storage by conversation depth — roughly 220 MB for a session whose actual content is 10 MB.

**Input manifests are deltas.** `base` + `appended`, which is exactly the continuity invariant, so
the representation *is* the claim. A full manifest appears only where the chain genuinely breaks:
compaction, fork, stream start. Those bases coincide with epoch boundaries.

**Containment is single-parent; causality is a sparse typed relation** carrying `via` and `quality`,
so an exact id match is distinguishable from an inference.

---

## 3. Continuous build

Rounds and segments are independent axes: the round is the **append** unit, the segment is the
**commit** unit. Many rounds accumulate while work continues; a segment closes only on an idle gap
plus gates. A segment close is recorded as an ordinary round — one that publishes the segment
boundary — not as a separate mechanism or a rewrite.

Watermark chain, each stage trailing the last:

```text
assembled_seq  <=  indexed_seq  <=  next_seq - 1
```

where `assembled_seq == conversation.state.through_seq`. A crash between publishing a round and
saving state leaves a round on disk that state does not mention, so **the filesystem is the
authority**: recovery lists the rounds directory and takes the highest, rather than trusting the
pointer.

**A per-stream record-id index must persist across commits.** A resume replays records whose first
occurrence may sit in an already-committed segment; without it, a replayed block lands as a
duplicate node. (This is the same rule the derived index enforces in `internal/index`: first
occurrence wins, and the duplicate is excluded from *every* lookup, not only from id resolution.)

**The invariant that keeps this honest:**

```text
fold(rounds 1..N)  ==  full parse of landed evidence 1..through_seqₙ
```

A full rebuild from the index is the reference implementation — simple and obviously correct — and
the fold is checked against it. Where they disagree the fold is wrong, and the disagreement is a
test failure.

The earlier draft asserted something weaker and wrong: that revision N+1 is a *prefix extension* of
revision N. It is not, and cannot be. Late evidence supersedes entities emitted rounds ago, which is
the whole point; a replay reordering after dedup or a compaction re-rooting an epoch changes what an
earlier round said. Prefix extension holds for the round *chain* — no round is ever rewritten — but
never for the folded result.

**Parser and policy versions are chain generations.** A change to either that alters meaning does not
retroactively fix earlier rounds and must not silently mix with them: it starts a new chain. Both
versions are in every header so a reader can detect the mix rather than fold through it.

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

A transient reference is promoted to permanent only on **terminal evidence** — a pruned source, a
session whose runtime wrote no such record. Never on elapsed rounds: "still open after N rounds" is
a statement about how often we looked, not about the data. That is why `state` has three values
(`open`, `resolved`, `terminal`) rather than a counter.

---

## 5. Open questions

1. **Does a compaction boundary force a segment close?** I lean no — epoch is model-context
   lifetime, segment is a commit window; conflating them makes segments hostage to Claude Code's
   compaction timing.
2. **Exact idle threshold and gate parameters**, given ~32% of >10-minute gaps are mid-turn.
3. **Where a session's conversation id comes from** when an application supplies one, and how it is
   recorded so cold reactivation preserves it.
4. **Retention now spans two directories.** Landed files under `data/<session>/` and rounds under
   `data/_conversations/<id>/` age independently, and a round references landed records by
   `(seq, row)`. Pruning landed data while keeping rounds leaves a structure whose content cannot be
   read; pruning rounds while keeping landed data is safe but discards the parse. There is no policy
   for this yet, and the coupling should be made explicit before either directory gets a TTL.
