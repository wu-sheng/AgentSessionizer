// Copyright 2026 The AgentSessionizer Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command asz is the AgentSessionizer command-line interface.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wu-sheng/AgentSessionizer/internal/adapters/claudecode"
	"github.com/wu-sheng/AgentSessionizer/internal/config"
	"github.com/wu-sheng/AgentSessionizer/internal/index"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
	"github.com/wu-sheng/AgentSessionizer/internal/verify"
	"github.com/wu-sheng/AgentSessionizer/pkg/record"
)

const usage = `asz - AgentSessionizer: conversation-level observability for long-lived AI agents

Usage:
  asz sources [-config FILE]        list discovered sessions and their sources
  asz collect [-config FILE] [-once]  land new source data into the storage root
  asz index [-config FILE] [SESSION]  report what the derived index holds
  asz show [-config FILE] SESSION ID   resolve a record id or tool-use id to its payload
  asz verify [-config FILE] [SESSION]  check landed data is contiguous and intact

Flags:
  -config FILE   configuration file (default: built-in defaults)
  -once          single pass, then exit (overrides collector.mode)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	once := fs.Bool("once", false, "single pass, then exit")
	_ = fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}

	var run func(*config.Config, config.Adapter, bool) error
	switch cmd {
	case "sources":
		run = cmdSources
	case "collect":
		run = cmdCollect
	case "index":
		run = cmdIndex
	case "show":
		run = cmdShow
	case "verify":
		run = cmdVerify
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	for _, ad := range cfg.Adapters {
		if !ad.Enabled {
			continue
		}
		if ad.Name != config.AdapterClaudeCodeLocal {
			fmt.Fprintf(os.Stderr, "skipping unknown adapter %q\n", ad.Name)
			continue
		}
		if err := run(cfg, ad, *once); err != nil {
			fatal(err)
		}
	}
}

func cmdSources(_ *config.Config, ad config.Adapter, _ bool) error {
	root, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	all, err := claudecode.Discover(root)
	if err != nil {
		return err
	}
	m := claudecode.NewMatcher(ad.Include, ad.Exclude)
	var sessions []claudecode.Session
	for _, s := range all {
		if m.Match(s) {
			sessions = append(sessions, s)
		}
	}
	fmt.Printf("source root: %s\n", root)
	if n := len(all) - len(sessions); n > 0 {
		fmt.Printf("filtered   : %d session(s) excluded by config\n", n)
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tDIRS\tSTREAMS\tMETA\tJOURNAL\tMANIFEST")
	var totalStreams int
	for _, s := range sessions {
		var streams, metas, journals, manifests int
		for _, src := range s.Sources {
			switch src.Kind {
			case claudecode.SrcMainTranscript, claudecode.SrcAgentTranscript:
				streams++
			case claudecode.SrcAgentMeta:
				metas++
			case claudecode.SrcJournal:
				journals++
			case claudecode.SrcWorkflowManifest:
				manifests++
			}
		}
		totalStreams += streams
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", s.ID, len(s.Dirs), streams, metas, journals, manifests)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d sessions, %d streams\n", len(sessions), totalStreams)

	// A session spread over several source directories is normal - a child
	// agent running in a different working directory is filed under that
	// directory instead. Surfacing it makes the session-first grouping visible.
	var multi []string
	for _, s := range sessions {
		if len(s.Dirs) > 1 {
			multi = append(multi, fmt.Sprintf("  %s  (%d dirs)", s.ID, len(s.Dirs)))
		}
	}
	if len(multi) > 0 {
		sort.Strings(multi)
		fmt.Printf("\n%d session(s) span more than one source directory:\n", len(multi))
		for _, m := range multi {
			fmt.Println(m)
		}
	}
	return nil
}

// cmdIndex reports what the derived index holds, which is the surface the
// assembler resolves structure against.
func cmdIndex(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	var want string
	if fsArgs := flag.CommandLine.Args(); len(fsArgs) > 0 {
		want = fsArgs[0]
	}
	for _, a := range os.Args[2:] {
		if !strings.HasPrefix(a, "-") {
			want = a
		}
	}

	sessions, err := os.ReadDir(zoneRoot)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tENTRIES\tBLOCKS\tSTREAMS\tSTRINGS\tINDEXED_SEQ")
	var totE, totB int
	var detail *index.Index
	var detailID string
	for _, d := range sessions {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		if want != "" && id != want {
			continue
		}
		ix, ok, err := index.Load(z.IndexDir(id), id)
		if err != nil || !ok {
			continue
		}
		stt, _ := storage.LoadIndexState(z.IndexStatePath(id), id)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", id, len(ix.Entries), len(ix.Blocks),
			len(ix.Streams()), ix.Strings.Len(), stt.IndexedSeq)
		totE += len(ix.Entries)
		totB += len(ix.Blocks)
		if want != "" {
			detail, detailID = ix, id
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\ntotal: %d entries, %d joinable blocks\n", totE, totB)

	if detail != nil {
		printIndexDetail(detail, detailID)
	}
	return nil
}

func printIndexDetail(ix *index.Index, id string) {
	ix.Build()
	names := map[index.Kind]string{
		index.KindUser: "user", index.KindAssistant: "assistant",
		index.KindAttachment: "attachment", index.KindSystem: "system",
		index.KindMeta: "agent_meta", index.KindJournal: "journal",
		index.KindManifest: "manifest", index.KindScript: "script",
		index.KindOther: "other", index.KindUnknown: "unknown",
	}
	byKind := map[index.Kind]int{}
	msgs := map[uint32]int{}
	var withCall, withRecord, withCycle int
	for i := range ix.Entries {
		e := &ix.Entries[i]
		byKind[e.Kind]++
		if e.Record != 0 {
			withRecord++
		}
		if e.Call != 0 {
			withCall++
			msgs[e.Call]++
		}
		if e.Cycle != 0 {
			withCycle++
		}
	}
	fmt.Printf("\n%s\n\nrecord kinds:\n", id)
	type kv struct {
		k index.Kind
		n int
	}
	var ks []kv
	for k, n := range byKind {
		ks = append(ks, kv{k, n})
	}
	sort.Slice(ks, func(a, b int) bool { return ks[a].n > ks[b].n })
	for _, x := range ks {
		fmt.Printf("  %-12s %8d\n", names[x.k], x.n)
	}

	var tu, tr int
	toolNames := map[uint32]int{}
	for i := range ix.Blocks {
		if b := &ix.Blocks[i]; b.Kind == index.BlockToolUse {
			tu++
			toolNames[b.Name]++
		} else if b.Kind == index.BlockToolResult {
			tr++
		}
	}
	fmt.Printf("\njoinable blocks:\n  tool_use     %8d\n  tool_result  %8d\n", tu, tr)
	if len(msgs) > 0 {
		fmt.Printf("\nprovider calls: %d distinct message ids over %d fragment records (mean %.2f)\n",
			len(msgs), withCall, float64(withCall)/float64(len(msgs)))
	}
	fmt.Printf("\nidentity coverage: uuid=%d  agentId=%d  of %d entries\n",
		withRecord, withCycle, len(ix.Entries))

	type tn struct {
		n    int
		name string
	}
	var tns []tn
	for tid, n := range toolNames {
		tns = append(tns, tn{n, ix.Strings.String(tid)})
	}
	sort.Slice(tns, func(a, b int) bool { return tns[a].n > tns[b].n })
	fmt.Println("\ntop tools:")
	for i, t := range tns {
		if i >= 8 {
			break
		}
		fmt.Printf("  %-14s %7d\n", t.name, t.n)
	}

	// A sample of entries, showing that one index spans every stream and every
	// landed sequence in the session.
	fmt.Println("\nsample entries (one index, all streams, all rounds):")
	et := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(et, "  SEQ\tROW\tKIND\tSTREAM\tRECORD\tCALL")
	shown, perStream := 0, map[uint32]int{}
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if perStream[e.Stream] >= 2 || shown >= 12 {
			continue
		}
		perStream[e.Stream]++
		shown++
		fmt.Fprintf(et, "  %d\t%d\t%s\t%s\t%s\t%s\n",
			e.Seq, e.Row, names[e.Kind],
			trunc(ix.Strings.String(e.Stream), 18),
			trunc(ix.Strings.String(e.Record), 13),
			trunc(ix.Strings.String(e.Call), 16))
	}
	_ = et.Flush()
}

func trunc(s string, n int) string {
	if s == "" {
		return "-"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// cmdShow resolves an identifier through the index to the landed record it
// names, and prints the payload.
//
// This is the materialisation path in miniature: the index answers WHERE a
// record is - which landed file, which line - and only then are the bytes read.
// cmdVerify checks that landed data is contiguous and intact, using only the
// landed files - the source is usually gone by the time anyone asks.
func cmdVerify(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	var want string
	for _, a := range os.Args[2:] {
		if !strings.HasPrefix(a, "-") {
			want = a
		}
	}
	entries, err := os.ReadDir(zoneRoot)
	if err != nil {
		return err
	}

	var sessions, streams, records, problems int
	for _, d := range entries {
		if !d.IsDir() || (want != "" && d.Name() != want) {
			continue
		}
		rep, err := verify.Session(z, d.Name())
		if err != nil {
			return err
		}
		if len(rep.Streams) == 0 {
			continue
		}
		sessions++
		streams += len(rep.Streams)
		records += rep.Records
		problems += rep.Problems
		if !rep.OK() {
			fmt.Printf("%s: %d problem(s)\n", rep.Session, rep.Problems)
			for _, line := range rep.Details() {
				fmt.Printf("   %s\n", line)
			}
		}
	}

	fmt.Printf("checked %d session(s), %d stream(s), %d records\n", sessions, streams, records)
	if problems > 0 {
		return fmt.Errorf("%d contiguity or integrity problem(s)", problems)
	}
	fmt.Println("all landed data is contiguous and matches its digests")
	return nil
}

func cmdShow(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	var args []string
	for _, a := range os.Args[2:] {
		if !strings.HasPrefix(a, "-") {
			args = append(args, a)
		}
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: asz show SESSION ID   (a record uuid or a tool_use id)")
	}
	session, want := args[0], args[1]

	ix, ok, err := index.Load(z.IndexDir(session), session)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no index for session %s", session)
	}

	// A tool-use id resolves to every block carrying it - normally the call and
	// its result, which is the join in its most direct form.
	if blocks := ix.ToolBlocks(want); len(blocks) > 0 {
		fmt.Printf("tool-use id %s resolves to %d block(s)\n\n", want, len(blocks))
		for _, b := range blocks {
			e := &ix.Entries[b.Entry]
			kind := "tool_use"
			if b.Kind == index.BlockToolResult {
				kind = "tool_result"
			}
			fmt.Printf("── %s  block %d of record  stream=%s\n", kind, b.Ord, ix.Strings.String(e.Stream))
			if err := printEntry(z, ix, session, e); err != nil {
				return err
			}
		}
		return nil
	}

	e, found := ix.EntryByRecord(want)
	if !found {
		return fmt.Errorf("id %q not found in the index for %s", want, session)
	}
	return printEntry(z, ix, session, e)
}

// printEntry locates one indexed record on disk and prints it.
func printEntry(z *storage.Zone, ix *index.Index, session string, e *index.Entry) error {
	stream := ix.Strings.String(e.Stream)
	run := ix.Strings.String(e.Run)
	dir := z.StreamDir(session, stream)
	if run != "" {
		dir = z.RunDir(session, run)
	}

	// seq selects the landed file; row selects the line within it.
	path, err := landedPathForSeq(dir, e.Seq)
	if err != nil {
		return err
	}
	fmt.Printf("   index : seq=%d row=%d\n", e.Seq, e.Row)
	fmt.Printf("   file  : %s\n", strings.TrimPrefix(path, z.Root()+"/"))

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := record.NewReader(f)
	if err != nil {
		return err
	}
	for row := uint32(1); ; row++ {
		rec, rerr := r.Next()
		if rerr != nil {
			return fmt.Errorf("row %d not found in %s", e.Row, path)
		}
		if row != e.Row {
			continue
		}
		body, berr := rec.SourceBytes()
		if berr != nil {
			return berr
		}
		fmt.Printf("   source: ord=%d off=%d sha=%s   (from the record's envelope)\n",
			rec.Ord, rec.Off, rec.Sha)
		if len(body) > 600 {
			fmt.Printf("   payload (%d bytes, truncated):\n   %s…\n\n", len(body), body[:600])
		} else {
			fmt.Printf("   payload (%d bytes):\n   %s\n\n", len(body), body)
		}
		return nil
	}
}

// landedPathForSeq finds the landed file carrying a sequence.
func landedPathForSeq(dir string, seq uint32) (string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	suffix := fmt.Sprintf("-%06d.jsonl", seq)
	for _, it := range items {
		if strings.HasSuffix(it.Name(), suffix) {
			return filepath.Join(dir, it.Name()), nil
		}
	}
	return "", fmt.Errorf("no landed file with seq %d in %s", seq, dir)
}

func cmdCollect(cfg *config.Config, ad config.Adapter, once bool) error {
	root, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(zoneRoot, 0o755); err != nil {
		return err
	}
	col := claudecode.New(root, storage.NewZone(zoneRoot), ad.Collector.MaxDeltaBytes)
	match := claudecode.NewMatcher(ad.Include, ad.Exclude).Match

	fmt.Printf("source root : %s\nstorage root: %s\n", root, zoneRoot)

	pass := func() error {
		start := time.Now()
		st, err := col.CollectAll(match)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] sessions=%d sources=%d landed=%d records=%d bytes=%s indexed=%d gone=%d conflicts=%d busy=%d pending=%d errors=%d (%s)\n",
			time.Now().Format("15:04:05"), st.Sessions, st.SourcesSeen, st.SourcesLanded,
			st.Records, humanBytes(st.Bytes), st.Indexed, st.SourcesGone, st.Conflicts,
			st.Busy, st.Pending, len(st.Errors), time.Since(start).Round(time.Millisecond))
		for _, e := range st.Errors {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
		if !st.Complete() {
			// A pass that under-collected must not look like a clean one.
			return fmt.Errorf("pass incomplete: %d source(s) still pending, %d error(s)",
				st.Pending, len(st.Errors))
		}
		return nil
	}

	if once || ad.Collector.Mode == config.ModeOnce {
		return pass()
	}
	for {
		if err := pass(); err != nil {
			return err
		}
		time.Sleep(ad.Collector.Interval)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
	os.Exit(1)
}
