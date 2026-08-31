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

package local_test

import (
	"sync"
	"testing"

	"github.com/wu-sheng/AgentSessionizer/internal/adapters/claudecode"
	"github.com/wu-sheng/AgentSessionizer/internal/storage"
)

// TestSharedCollectorIsSafe runs two passes concurrently on ONE Collector.
// Per-session state on the shared struct would race here; the -race detector
// is the assertion.
func TestSharedCollectorIsSafe(t *testing.T) {
	src := t.TempDir()
	x := newTranscript(t, src)
	x.AddTurn("do the thing", true)
	x.Flush()

	col := claudecode.New(src, storage.NewZone(t.TempDir()), 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = col.CollectAll(nil) }()
	}
	wg.Wait()
}
