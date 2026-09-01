# Design Plan 03 — Read and Export

**Status:** design in progress. Nothing here is implemented.
**Scope:** getting a conversation out of the round chain and in front of a reader. Collection is
Plan 01 and assembly is Plan 02; both are implemented and neither changes.

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

## 3. The contract

*To be filled in from the design work in progress.*

---

## 4. Open questions

1. **Retention across two directories.** Carried over from Plan 02 and now sharper: a round
   references landed records, so pruning landed data while keeping rounds leaves a structure whose
   content cannot be read. A reader meeting that must be told the content is gone, not shown an
   empty one — which means content state has to reach the screen.
2. **Reading a conversation that is still being written.** `unmeasured`. Collection and parse both
   run while someone could be reading.
3. **Search.** Nothing indexes text today; the derived index holds identifiers only, deliberately.
