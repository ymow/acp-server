package gittwin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GitHubEventKind is the normalized event category exposed to the bridge.
// Keep this shorter than the raw GitHub event × action matrix so the
// mapping layer (cmd/acp-git-bridge) can switch on a single token.
type GitHubEventKind string

const (
	EventPullRequestOpened   GitHubEventKind = "pull_request.opened"
	EventPullRequestMerged   GitHubEventKind = "pull_request.merged"
	EventPullRequestRejected GitHubEventKind = "pull_request.rejected" // closed without merge
	EventPushProtected       GitHubEventKind = "push.protected"         // push to protected branch, not force
	EventPushForced          GitHubEventKind = "push.forced"            // force-push or rebase
	EventTagSettlement       GitHubEventKind = "tag.settlement"         // tag name prefixed settlement-*
	EventIgnored             GitHubEventKind = "ignored"
)

// VerifyGitHubSignature checks the X-Hub-Signature-256 header (format "sha256=<hex>")
// against HMAC-SHA256(secret, body). ACR-400 Part 7: Bridge must validate this
// before any parsing. Returns ErrBadSignature on mismatch.
var ErrBadSignature = errors.New("webhook signature mismatch")

func VerifyGitHubSignature(body []byte, header, secret string) error {
	if secret == "" {
		return errors.New("webhook secret not configured")
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return ErrBadSignature
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := mac.Sum(nil)
	if !hmac.Equal(got, want) {
		return ErrBadSignature
	}
	return nil
}

// ParsedEvent is the bridge-facing projection of a GitHub webhook payload.
// It intentionally carries only the fields the ACR-400 v0.1 mapping needs.
type ParsedEvent struct {
	Kind       GitHubEventKind
	RepoURL    string          // https://github.com/owner/repo
	CommitHead string          // HEAD commit sha after the event
	// PR fields (populated only for pull_request.*)
	PRNumber    int
	PRURL       string
	PRTitle     string
	PRAuthor    string // GitHub login
	PRBaseRef   string
	PRHeadRef   string
	PRMergedBy  string // GitHub login of merger, if kind == merged
	PRAdditions int    // lines added across the PR (GitHub pull_request payload)
	PRDeletions int    // lines removed across the PR
	PRChangedFiles int // number of files touched
	// Push fields (populated only for push.*)
	PushRef     string
	PushForced  bool
	PushAuthors []string // GitHub logins from commit authors in the push
	// Tag fields (populated only for tag.settlement)
	TagName string
	// Raw gives the mapper access to anything we didn't pre-extract.
	Raw json.RawMessage
}

// ParseGitHubEvent dispatches on the "X-GitHub-Event" header + payload shape
// and returns a ParsedEvent. Unsupported events return (ParsedEvent{Kind: EventIgnored}, nil).
func ParseGitHubEvent(eventHeader string, body []byte) (ParsedEvent, error) {
	p := ParsedEvent{Kind: EventIgnored, Raw: body}

	switch eventHeader {
	case "pull_request":
		return parsePullRequest(body)
	case "push":
		return parsePush(body)
	case "create":
		return parseCreate(body)
	case "ping":
		return p, nil
	default:
		return p, nil
	}
}

// --- pull_request ---------------------------------------------------------

type prPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
		Merged  bool   `json:"merged"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		MergedBy *struct {
			Login string `json:"login"`
		} `json:"merged_by"`
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
		ChangedFiles int `json:"changed_files"`
	} `json:"pull_request"`
	Repository struct {
		HTMLURL string `json:"html_url"`
	} `json:"repository"`
}

func parsePullRequest(body []byte) (ParsedEvent, error) {
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ParsedEvent{}, fmt.Errorf("parse pull_request: %w", err)
	}
	ev := ParsedEvent{
		RepoURL:        p.Repository.HTMLURL,
		PRNumber:       p.Number,
		PRURL:          p.PullRequest.HTMLURL,
		PRTitle:        p.PullRequest.Title,
		PRAuthor:       p.PullRequest.User.Login,
		PRBaseRef:      p.PullRequest.Base.Ref,
		PRHeadRef:      p.PullRequest.Head.Ref,
		CommitHead:     p.PullRequest.Head.SHA,
		PRAdditions:    p.PullRequest.Additions,
		PRDeletions:    p.PullRequest.Deletions,
		PRChangedFiles: p.PullRequest.ChangedFiles,
		Raw:            body,
	}
	switch p.Action {
	case "opened", "reopened":
		ev.Kind = EventPullRequestOpened
	case "closed":
		if p.PullRequest.Merged {
			ev.Kind = EventPullRequestMerged
			if p.PullRequest.MergedBy != nil {
				ev.PRMergedBy = p.PullRequest.MergedBy.Login
			}
		} else {
			ev.Kind = EventPullRequestRejected
		}
	default:
		ev.Kind = EventIgnored
	}
	return ev, nil
}

// --- push -----------------------------------------------------------------

type pushPayload struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Forced  bool   `json:"forced"`
	Commits []struct {
		ID     string `json:"id"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	} `json:"commits"`
	Repository struct {
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

func parsePush(body []byte) (ParsedEvent, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ParsedEvent{}, fmt.Errorf("parse push: %w", err)
	}
	ev := ParsedEvent{
		RepoURL:    p.Repository.HTMLURL,
		CommitHead: p.After,
		PushRef:    p.Ref,
		PushForced: p.Forced,
		Raw:        body,
	}
	for _, c := range p.Commits {
		if c.Author.Username != "" {
			ev.PushAuthors = append(ev.PushAuthors, c.Author.Username)
		}
	}
	// ACR-400 Part 6: force-push never drives ledger mutation.
	if p.Forced {
		ev.Kind = EventPushForced
		return ev, nil
	}
	// v0.1 scope: only pushes to the default branch matter. Feature-branch
	// pushes are picked up via the pull_request event stream instead.
	if p.Ref == "refs/heads/"+p.Repository.DefaultBranch {
		ev.Kind = EventPushProtected
	} else {
		ev.Kind = EventIgnored
	}
	return ev, nil
}

// --- create (tag) ---------------------------------------------------------

type createPayload struct {
	Ref        string `json:"ref"`
	RefType    string `json:"ref_type"`
	Repository struct {
		HTMLURL string `json:"html_url"`
	} `json:"repository"`
}

func parseCreate(body []byte) (ParsedEvent, error) {
	var p createPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ParsedEvent{}, fmt.Errorf("parse create: %w", err)
	}
	ev := ParsedEvent{
		RepoURL: p.Repository.HTMLURL,
		Raw:     body,
	}
	if p.RefType == "tag" && strings.HasPrefix(p.Ref, "settlement-") {
		ev.Kind = EventTagSettlement
		ev.TagName = p.Ref
		return ev, nil
	}
	ev.Kind = EventIgnored
	return ev, nil
}
