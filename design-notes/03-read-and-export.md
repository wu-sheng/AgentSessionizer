# Design Plan 03 — Read and Export

**Status:** design in progress. Nothing here is implemented.
**Scope:** getting a conversation out of the round chain and in front of a reader. Collection is
Plan 01 and assembly is Plan 02; both are implemented and neither changes.

The plan changed shape once already. It began as "export and preview" and the first real question -
what does a viewer read? - answered itself immediately: it reads ASB, because a second format would
only drift from the first. The interesting question turned out to be the one behind it, in
[section 3](#3-the-contract-is-asb-and-that-exposes-a-hole): the model is runtime-agnostic about
structure and not about content, and a viewer is the first thing that would notice.

Every number below was measured on the same corpus Plans 01 and 02 were measured on: 62 sessions,
1,100.1 MB of landed records, 319,264 assembled nodes. Where a number is missing it says
`unmeasured` rather than an estimate.

---

## 0. Why this plan exists

**Nothing has read the output yet.** Plan 02 produced 319,264 nodes across 62 sessions and no reader
has looked at one. That matters more than it sounds: most of the model's decisions were justified by
a claim about reading — a tool use is one step because *a reader thinks of it as one event*, a child
agent's output belongs to the child because otherwise *a rendered conversation shows the same work
twice*, a delegation stays inside one Talk because *a reader sees one interaction*. Those are
falsifiable claims about reading, and only a reader can falsify them.

So this plan's real output is not a viewer. It is the set of decisions a viewer forces.

### Three things a reader needs that the chain does not carry

**1. There is no time anywhere.** Measured: 0 of 319,264 nodes carry any temporal field.

```text
100%  id · kind · revision
 99%  parent · ref · stream
 64%  attrs
  0%  anything temporal
```

This is not an oversight. A round is deliberately time-free so that the same evidence reproduces
the same bytes and the same digest. But a node's *observed* time — when the runtime says the thing
happened — comes from a landed record that is immutable, so it is just as reproducible as the
node's parent or kind. It could be carried; it is not. A viewer cannot draw a timeline, group by
day, or say "twenty minutes later" without opening every referenced record.

**2. A Talk has no name.** A reader opening a session sees a list of ids. The largest session has
922 of them. Nothing in the chain distinguishes one from another.

**3. Nothing resolves a reference to bytes.** A node says `{"seq":1,"row":1004}`. The only thing
that turns that into text is `asz show`, which prints one record. There is no path from a folded
conversation to what anybody said.

### And one the maintainer raised

**A viewer's fold is not the assembler's fold.** The assembler applies every round in order and
keeps the result. A viewer may want the state as of an *earlier* round, or the *history* of one
entity across rounds, or to render rounds as they *arrive* rather than reading them all first. The
rounds hold enough to answer any of these. Which of them to serve, and how, is undecided.

---

## 1. Where the bytes are

The whole corpus, by what the bytes actually contain:

| MB | share | what |
| ---: | ---: | --- |
| 370.0 | 33.6% | scaffolding — record ids, usage blocks, envelope |
| 334.8 | 30.4% | tool response, content block |
| 146.4 | 13.3% | **thinking signatures** |
| 83.8 | 7.6% | tool response, **a structured duplicate of the same output** |
| 75.0 | 6.8% | tool command |
| 58.7 | 5.3% | injected context |
| 18.4 | 1.7% | message text |
| 14.3 | 1.3% | assistant text |
| 0.1 | 0.0% | **thinking text** |

Four readings of that table, each of which constrains the contract:

**Tool responses are 38.1% and are 6× the size of the commands.** Asking what a tool *did* is cheap;
carrying what it *returned* is the whole problem.

**What a person came to read is 3.0%.** Message text plus assistant text is 32.7 MB of 1,100 MB. The
conversation as read is a rounding error on the conversation as stored.

**13.3% is cryptographic signature on thinking blocks whose text is 0.1 MB.** A signature exists so
a provider can verify a block handed back to it. It means nothing to a reader, and it is the second
largest single category in the corpus.

**7.6% is the same tool output stored a second time.** A main-stream tool result carries both the
content block and a structured form with `stdout`/`stderr` split out. Both are real — the structured
one has detail the block lacks — but a reader wants one of them.

### The share is not stable, which rules out a fixed policy

Per session, over the 28 sessions larger than 1 MB:

```text
tool-response share    min 17.3%    median 35.1%    max 83.5%
```

A rule tuned to the median is wrong by a factor of two at both ends. Whatever the contract does
about tool output, it cannot be a constant.

### Compression is not the answer either

| | size | ratio |
| --- | ---: | ---: |
| largest session, landed | 200 MB | — |
| zip / gzip | 68 MB | 2.8× |
| zstd -19 | 45 MB | 4.3× |
| **its round chain** | **21 MB → 1.4 MB** | **15×** |

Rounds compress fifteen times because they are tens of thousands of near-identical structured lines.
Landed content does not, because a third of it is high-entropy signature and captured output.

Removing the two known-wasteful categories *before* compressing beats compressing harder: 45.2 MB →
26.9 MB, a 40% cut that no compression level recovers on its own.

---

## 2. What a reader asks for

A Talk is the natural unit — it is the thing a reader opens. Measured across all 4,941 of them,
structure only, no content:

```text
one talk's subtree     p50   12.7 KB    42 nodes
                       p90   43.4 KB   140 nodes
                       p99  111.1 KB   348 nodes
                       max  413.8 KB  1,475 nodes
```

The median talk's structure fits in a network round trip. The whole largest session is 16 MB of
structure — too much for a first screen, fine for a background load.

That asymmetry is the shape of the answer: **structure is small enough to be eager, content is not.**

---

## 3. The contract is ASB, and that exposes a hole

**A viewer reads ASB. It does not read landed files, and it does not read a new format invented for
it.** The round chain already is the read contract: it carries the structure, it references the
content, and it is verifiable on its own. Inventing a second shape for viewers would mean two things
to keep in step, and the second would be the one that drifts.

But ASB references content; it does not describe it. And that is where the layering breaks.

### 3.1 What is runtime-agnostic today, and what is not

The project's rule is that a runtime's vocabulary stops at its adapter. Measured against the code,
that is half true:

| | who interprets it | runtime-agnostic? |
| --- | --- | --- |
| **structure** | the adapter maps its identifiers onto index roles | **yes** |
| **content** | nobody — the payload is the source bytes, untouched | **no** |

Landing preserves payloads byte-for-byte, on purpose: it is what makes a landed record replayable
and its digest meaningful. The consequence is that the only description of what is *inside* a
payload is the runtime's own schema.

So a viewer rendering one `message.assistant` node has to do this:

```text
node.ref {seq:1, row:1004}
  -> landed record
    -> payload.message.content[0].text        <- Claude Code's shape, not the model's
```

Nothing stops it, and nothing tells it that walking `.message.content[]` is an adapter's business.
The first viewer that ships would hard-code one agent product's JSON into the reader, and the
model's whole claim to be runtime-agnostic would be true only of the parts nobody looks at.

**There is also no adapter interface at all.** `internal/adapters/claudecode` is a package of free
functions that `cmd/asz` calls by name. Nothing states what an adapter must provide, so a second one
has nothing to implement against.

### 3.2 Two axes, currently collapsed into one

A landed header carries a single `adapter` string, `claude-code-local/0.1.0`. It is doing two jobs:

- **transport** — how records arrive: reading local files, or receiving a push
- **dialect** — how records are interpreted: Claude Code's schema, Codex's, another agent's

Those vary independently. A push receiver for Claude Code shares every interpretation rule with the
local reader and no acquisition code. A local reader for Codex shares the acquisition shape and no
interpretation rules.

```text
                    dialect: claude-code    dialect: codex     dialect: …
transport: local    implemented             —                  —
transport: push     —                       —                  —
```

Keeping the *transport* in the adapter name is right and already documented: a push adapter is a
separate entry rather than a mode flag, because the collection posture changes what can be promised
about completeness. Keeping the *dialect* there too is what makes the table above impossible to
fill in without duplication.

### 3.3 The surface an adapter has to provide

Three responsibilities, of which only the first two exist and only one is named:

| | today | needed |
| --- | --- | --- |
| **acquire** — get records into the zone | `claudecode.Collector`, called directly | a transport interface; pull and push are two implementations |
| **interpret structure** — identifiers onto roles | `claudecode.IndexEntry`, a free function | a dialect interface, dispatched by the landed header |
| **interpret content** — a payload into something readable | *missing* | a dialect method returning a runtime-agnostic content unit |

The third is the one a viewer is blocked on. Its shape is constrained by things already measured:

- It must be able to return **part** of a payload. A single tool result runs to 1.1 MB, and the
  tool-response share of a session varies from 17.3% to 83.5%, so truncation cannot be a caller's
  afterthought.
- It must report **content state**, not just bytes. `available`, `truncated`, `redacted`,
  `unavailable` are already in the model; a reader has to be told which one it got, or the model's
  honesty rule stops at the last hop.
- It must be addressable by exactly what a node carries: a landed record and an optional block
  ordinal. Nothing else is in scope, because nothing else is in an ASB reference.

An adapter that cannot interpret a payload it produced is still useful — the structure is already
assembled — so this is `unavailable` rather than an error, and a reader is told the content cannot
be described rather than shown a guess.

### 3.4 What this changes about the plan

The viewer is no longer the first thing to build, and it was never the point. **The adapter contract
is,** because a viewer written before it exists will encode Claude Code into the reader, and the
second agent product will find that out the expensive way.

It also reframes what Plan 03 is for. It is not "export and preview". It is the point at which the
model has to be honest about content the way it is already honest about structure.

*The rest of the contract — the fetch granularity, the viewer-side fold, whether a node carries its
observed time — is still being designed.*

---

## 4. Open questions

1. **Retention across two directories.** Carried over from Plan 02 and now sharper: a round
   references landed records, so pruning landed data while keeping rounds leaves a structure whose
   content cannot be read. A reader meeting that must be told the content is gone, not shown an
   empty one — which means content state has to reach the screen.
2. **Reading a conversation that is still being written.** `unmeasured`. Collection and parse both
   run while someone could be reading.
3. **Search.** Nothing indexes text today; the derived index holds identifiers only, deliberately.
4. **Where the dialect is recorded.** A landed header carries one `adapter` string that names both
   transport and dialect. Interpretation needs the dialect alone. Deriving it from the name by
   convention works for the one adapter that exists and will not survive the second. Adding a field
   changes an envelope that is already on disk in immutable files, so the migration has to be
   thought through rather than assumed: the envelope is versioned (`h:1`), and landed files are
   never rewritten, so a reader would have to handle both. `unmeasured`: whether any existing
   landed data would need a compatibility path, or whether re-collection is acceptable.
5. **Whether a runtime's own schema changes under us.** Claude Code's record shape is not a
   published contract and has already shifted during this project. Structure survives that - the
   adapter absorbs a rename - but content interpretation is a second surface with the same
   exposure, and nothing detects a shape change today. A dialect that meets a payload it does not
   recognise must report `unavailable` rather than guess, which is the same rule the rest of the
   model follows.
