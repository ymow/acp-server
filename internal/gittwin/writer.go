package gittwin

// AnchorWriter turns a pending git_twin_anchors row into an actual git note
// on refs/notes/acp-anchors. ACR-400 Part 5: the bridge owns the git side,
// the server owns the ledger side; this file is the bridge's executor.
//
// Layering: the writer shells out to the system `git` binary rather than
// linking against go-git, because the operator already needs `git` for the
// push credentials dance (ssh-agent, credential helpers, etc.) and using
// the shelled binary inherits their config for free. Tests inject a fake
// GitRunner; production uses execGit.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NotesRef is the canonical ACR-400 anchor ref. Kept as a const so any ad-hoc
// tool that needs to read anchors resolves to the same ref name.
const NotesRef = "refs/notes/acp-anchors"

// GitRunner runs a single git invocation in the given directory. Splitting
// this out makes AnchorWriter trivially testable with a recorded-call fake.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// AnchorJob is the minimum payload the writer needs from a git_twin_anchors row.
type AnchorJob struct {
	AnchorID string
	RepoURL  string
	NoteBody string // pre-rendered JSON from Anchor.NoteBody
}

// AnchorWriter writes anchor notes for pending jobs. CacheDir is the parent
// directory under which per-repo clones live; NotesRef overrides are only
// for tests.
type AnchorWriter struct {
	CacheDir string
	Runner   GitRunner
	NotesRef string // defaults to NotesRef when empty
}

// Write ensures a clone of job.RepoURL exists under CacheDir, fetches the
// latest default branch, writes (or overwrites) a git note on the current
// HEAD, pushes refs/notes/acp-anchors to origin, and returns the notes-ref
// commit SHA for the server ack. Returned error means the server should
// keep the anchor pending and the bridge will retry on the next tick.
func (w *AnchorWriter) Write(ctx context.Context, job AnchorJob) (string, error) {
	if job.RepoURL == "" {
		return "", errors.New("anchor job missing repo_url")
	}
	if job.NoteBody == "" {
		return "", errors.New("anchor job missing note_body")
	}
	notesRef := w.NotesRef
	if notesRef == "" {
		notesRef = NotesRef
	}

	repoDir, err := w.ensureClone(ctx, job.RepoURL)
	if err != nil {
		return "", fmt.Errorf("ensure clone: %w", err)
	}

	// Fetch origin so we have the newest HEAD and any existing anchor notes.
	// Running fetch in a fresh clone is redundant but harmless and keeps the
	// cached-clone hot-path simple.
	if _, err := w.Runner.Run(ctx, repoDir, "fetch", "origin", "--prune"); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}
	// Pull any existing anchor notes so `git notes add -f` doesn't race a
	// peer bridge. Swallow failure because the ref may not yet exist.
	_, _ = w.Runner.Run(ctx, repoDir, "fetch", "origin",
		fmt.Sprintf("+%s:%s", notesRef, notesRef))

	head, err := w.resolveDefaultHead(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}

	// `git notes add -f` overwrites any pre-existing note on that commit.
	// Overwriting is safe: the server refuses conflicting re-acks, so if we
	// ever race a peer bridge the losing side surfaces via 409 on ack.
	if _, err := w.Runner.Run(ctx, repoDir, "notes", "--ref="+notesRef,
		"add", "-f", "-m", job.NoteBody, head); err != nil {
		return "", fmt.Errorf("git notes add: %w", err)
	}

	if _, err := w.Runner.Run(ctx, repoDir, "push", "origin", notesRef); err != nil {
		return "", fmt.Errorf("git push notes: %w", err)
	}

	out, err := w.Runner.Run(ctx, repoDir, "rev-parse", notesRef)
	if err != nil {
		return "", fmt.Errorf("rev-parse notes: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("rev-parse notes returned empty sha")
	}
	return sha, nil
}

// ensureClone creates a bare-ish clone the first time we see a repo URL,
// then reuses it for subsequent writes. We clone with `--no-checkout`
// because we never touch the working tree — only notes + fetch + push.
func (w *AnchorWriter) ensureClone(ctx context.Context, repoURL string) (string, error) {
	dir := filepath.Join(w.CacheDir, cloneDirName(repoURL))
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(w.CacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}
	// clone expects the parent to exist and the target to not exist.
	if _, err := w.Runner.Run(ctx, w.CacheDir,
		"clone", "--no-checkout", repoURL, filepath.Base(dir)); err != nil {
		return "", fmt.Errorf("git clone: %w", err)
	}
	return dir, nil
}

// resolveDefaultHead asks the remote for its HEAD pointer and resolves it
// locally. Using `symbolic-ref refs/remotes/origin/HEAD` avoids hard-coding
// `main` vs `master` — we use whatever the remote declares.
func (w *AnchorWriter) resolveDefaultHead(ctx context.Context, repoDir string) (string, error) {
	out, err := w.Runner.Run(ctx, repoDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		// Fall back to whatever origin/main or origin/master resolves to.
		for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
			if out, err2 := w.Runner.Run(ctx, repoDir, "rev-parse", ref); err2 == nil {
				return strings.TrimSpace(string(out)), nil
			}
		}
		return "", err
	}
	ref := strings.TrimSpace(string(out))
	out, err = w.Runner.Run(ctx, repoDir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func cloneDirName(repoURL string) string {
	h := sha1.Sum([]byte(repoURL))
	return hex.EncodeToString(h[:])[:16]
}

// execGit is the production GitRunner. It intentionally inherits env so
// ssh-agent, GIT_ASKPASS, and credential helpers all work unchanged.
type execGit struct{}

// NewExecGitRunner returns a GitRunner that shells out to the system git.
func NewExecGitRunner() GitRunner { return execGit{} }

func (execGit) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// DefaultCacheDir returns the per-user cache location under which cloned
// repos live. Falls back to the system temp dir when UserCacheDir fails.
func DefaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "acp-git-bridge", "repos")
}
