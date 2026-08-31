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
	"text/tabwriter"
	"time"

	"github.com/wu-sheng/AgentSessionizer/internal/adapters/claudecode"
	"github.com/wu-sheng/AgentSessionizer/internal/config"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
)

const usage = `asz - AgentSessionizer: conversation-level observability for long-lived AI agents

Usage:
  asz sources [-config FILE]        list discovered sessions and their sources
  asz collect [-config FILE] [-once]  land new source data into the storage root

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
		fmt.Printf("[%s] sessions=%d sources=%d landed=%d records=%d bytes=%s gone=%d conflicts=%d busy=%d pending=%d errors=%d (%s)\n",
			time.Now().Format("15:04:05"), st.Sessions, st.SourcesSeen, st.SourcesLanded,
			st.Records, humanBytes(st.Bytes), st.SourcesGone, st.Conflicts, st.Busy, st.Pending,
			len(st.Errors), time.Since(start).Round(time.Millisecond))
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
