package gittwin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every git invocation and returns programmed responses.
// Keyed by space-joined args; missing keys return ("", nil) which is the
// right default for commands whose output we don't care about.
type fakeRunner struct {
	dir      string
	calls    [][]string
	returns  map[string]fakeReturn
	hadClone bool
}

type fakeReturn struct {
	out []byte
	err error
}

func (f *fakeRunner) Run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.dir = dir
	f.calls = append(f.calls, append([]string{}, args...))
	key := strings.Join(args, " ")
	if ret, ok := f.returns[key]; ok {
		// If we're being asked to clone, also create the repo dir + .git so
		// the next ensureClone short-circuits.
		if args[0] == "clone" {
			target := filepath.Join(dir, args[len(args)-1], ".git")
			_ = os.MkdirAll(target, 0o755)
			f.hadClone = true
		}
		return ret.out, ret.err
	}
	if args[0] == "clone" {
		target := filepath.Join(dir, args[len(args)-1], ".git")
		_ = os.MkdirAll(target, 0o755)
		f.hadClone = true
	}
	return nil, nil
}

func TestAnchorWriterHappyPath(t *testing.T) {
	cache := t.TempDir()
	runner := &fakeRunner{
		returns: map[string]fakeReturn{
			"symbolic-ref refs/remotes/origin/HEAD": {out: []byte("refs/remotes/origin/main\n")},
			"rev-parse refs/remotes/origin/main":    {out: []byte("abc1234head\n")},
			"rev-parse " + NotesRef:                 {out: []byte("noteCommit7\n")},
		},
	}
	w := &AnchorWriter{CacheDir: cache, Runner: runner}

	sha, err := w.Write(context.Background(), AnchorJob{
		AnchorID: "anch_x",
		RepoURL:  "https://example.com/owner/repo.git",
		NoteBody: `{"type":"acp.anchor.settlement.v1"}`,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sha != "noteCommit7" {
		t.Fatalf("sha: want noteCommit7 got %q", sha)
	}
	if !runner.hadClone {
		t.Fatal("expected clone on first write")
	}

	// Verify command ordering: clone → fetch origin → fetch notes ref → symbolic-ref
	// → rev-parse HEAD → notes add -f → push → rev-parse notes.
	want := [][]string{
		{"clone", "--no-checkout", "https://example.com/owner/repo.git"},
		{"fetch", "origin", "--prune"},
		{"fetch", "origin"},
		{"symbolic-ref", "refs/remotes/origin/HEAD"},
		{"rev-parse", "refs/remotes/origin/main"},
		{"notes", "--ref=" + NotesRef, "add", "-f", "-m"},
		{"push", "origin", NotesRef},
		{"rev-parse", NotesRef},
	}
	if len(runner.calls) != len(want) {
		t.Fatalf("call count: want %d got %d (%v)", len(want), len(runner.calls), runner.calls)
	}
	for i, w := range want {
		got := runner.calls[i]
		for j, tok := range w {
			if j >= len(got) || !strings.HasPrefix(got[j], tok) && got[j] != tok {
				t.Fatalf("call %d arg %d: want prefix %q, got %v", i, j, tok, got)
			}
		}
	}
	// The note body should appear somewhere on the notes add call.
	notesCall := runner.calls[5]
	joined := strings.Join(notesCall, " ")
	if !strings.Contains(joined, "acp.anchor.settlement.v1") {
		t.Fatalf("notes add did not include body: %v", notesCall)
	}
}

func TestAnchorWriterSecondCallReusesClone(t *testing.T) {
	cache := t.TempDir()
	runner := &fakeRunner{
		returns: map[string]fakeReturn{
			"symbolic-ref refs/remotes/origin/HEAD": {out: []byte("refs/remotes/origin/main\n")},
			"rev-parse refs/remotes/origin/main":    {out: []byte("sha1\n")},
			"rev-parse " + NotesRef:                 {out: []byte("note1\n")},
		},
	}
	w := &AnchorWriter{CacheDir: cache, Runner: runner}
	job := AnchorJob{AnchorID: "a", RepoURL: "https://x/y.git", NoteBody: "{}"}

	if _, err := w.Write(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	firstCalls := len(runner.calls)
	if _, err := w.Write(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	// Second write should skip the clone, so strictly fewer calls on the
	// second pass than the first.
	if len(runner.calls) != firstCalls+(firstCalls-1) {
		t.Fatalf("second write should add firstCalls-1 (no clone), got total %d after first=%d",
			len(runner.calls), firstCalls)
	}
	for _, call := range runner.calls[firstCalls:] {
		if call[0] == "clone" {
			t.Fatal("second Write clone'd again")
		}
	}
}

func TestAnchorWriterRejectsEmptyJob(t *testing.T) {
	w := &AnchorWriter{CacheDir: t.TempDir(), Runner: &fakeRunner{}}
	if _, err := w.Write(context.Background(), AnchorJob{}); err == nil {
		t.Fatal("empty job should error")
	}
	if _, err := w.Write(context.Background(), AnchorJob{RepoURL: "x"}); err == nil {
		t.Fatal("missing note_body should error")
	}
}

func TestAnchorWriterPropagatesFetchError(t *testing.T) {
	cache := t.TempDir()
	runner := &fakeRunner{
		returns: map[string]fakeReturn{
			"fetch origin --prune": {err: errors.New("boom")},
		},
	}
	w := &AnchorWriter{CacheDir: cache, Runner: runner}
	_, err := w.Write(context.Background(), AnchorJob{
		AnchorID: "a", RepoURL: "https://x/y.git", NoteBody: "{}",
	})
	if err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Fatalf("expected fetch error, got %v", err)
	}
}
