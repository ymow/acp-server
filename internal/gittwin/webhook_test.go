package gittwin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func signBody(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyGitHubSignature(t *testing.T) {
	body := []byte(`{"zen":"ok"}`)
	sig := signBody("s3cret", body)
	if err := VerifyGitHubSignature(body, sig, "s3cret"); err != nil {
		t.Fatalf("valid sig rejected: %v", err)
	}
	if err := VerifyGitHubSignature(body, sig, "wrong"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret should error, got %v", err)
	}
	if err := VerifyGitHubSignature(body, "sha1=abc", "s3cret"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong prefix should error, got %v", err)
	}
	if err := VerifyGitHubSignature(body, sig, ""); err == nil {
		t.Fatal("empty secret should error")
	}
}

func TestParsePullRequestOpened(t *testing.T) {
	body := []byte(`{
		"action":"opened",
		"number":42,
		"pull_request":{
			"html_url":"https://github.com/o/r/pull/42",
			"title":"add feature",
			"merged":false,
			"user":{"login":"ymow"},
			"base":{"ref":"main"},
			"head":{"ref":"feat","sha":"abc123"}
		},
		"repository":{"html_url":"https://github.com/o/r"}
	}`)
	ev, err := ParseGitHubEvent("pull_request", body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventPullRequestOpened {
		t.Fatalf("kind=%s", ev.Kind)
	}
	if ev.PRNumber != 42 || ev.PRAuthor != "ymow" || ev.CommitHead != "abc123" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.PRURL != "https://github.com/o/r/pull/42" {
		t.Fatalf("pr url: %s", ev.PRURL)
	}
}

func TestParsePullRequestMerged(t *testing.T) {
	body := []byte(`{
		"action":"closed",
		"number":7,
		"pull_request":{
			"html_url":"https://github.com/o/r/pull/7",
			"merged":true,
			"user":{"login":"alice"},
			"merged_by":{"login":"reviewer"},
			"base":{"ref":"main"},
			"head":{"ref":"topic","sha":"def456"}
		},
		"repository":{"html_url":"https://github.com/o/r"}
	}`)
	ev, _ := ParseGitHubEvent("pull_request", body)
	if ev.Kind != EventPullRequestMerged {
		t.Fatalf("kind=%s", ev.Kind)
	}
	if ev.PRMergedBy != "reviewer" {
		t.Fatalf("merged_by=%s", ev.PRMergedBy)
	}
}

func TestParsePullRequestClosedUnmerged(t *testing.T) {
	body := []byte(`{
		"action":"closed",
		"number":8,
		"pull_request":{
			"html_url":"https://github.com/o/r/pull/8",
			"merged":false,
			"user":{"login":"bob"},
			"base":{"ref":"main"},
			"head":{"ref":"x","sha":"zzz"}
		},
		"repository":{"html_url":"https://github.com/o/r"}
	}`)
	ev, _ := ParseGitHubEvent("pull_request", body)
	if ev.Kind != EventPullRequestRejected {
		t.Fatalf("kind=%s", ev.Kind)
	}
}

func TestParsePushForcedIsBlocked(t *testing.T) {
	body := []byte(`{
		"ref":"refs/heads/main",
		"before":"aaa",
		"after":"bbb",
		"forced":true,
		"repository":{"html_url":"https://github.com/o/r","default_branch":"main"}
	}`)
	ev, _ := ParseGitHubEvent("push", body)
	if ev.Kind != EventPushForced {
		t.Fatalf("force-push must map to EventPushForced, got %s", ev.Kind)
	}
}

func TestParsePushToDefaultBranch(t *testing.T) {
	body := []byte(`{
		"ref":"refs/heads/main",
		"after":"bbb",
		"forced":false,
		"commits":[{"id":"c1","author":{"username":"ymow"}}],
		"repository":{"html_url":"https://github.com/o/r","default_branch":"main"}
	}`)
	ev, _ := ParseGitHubEvent("push", body)
	if ev.Kind != EventPushProtected {
		t.Fatalf("kind=%s", ev.Kind)
	}
	if len(ev.PushAuthors) != 1 || ev.PushAuthors[0] != "ymow" {
		t.Fatalf("authors=%v", ev.PushAuthors)
	}
}

func TestParsePushToFeatureBranchIsIgnored(t *testing.T) {
	body := []byte(`{
		"ref":"refs/heads/topic",
		"forced":false,
		"repository":{"html_url":"https://github.com/o/r","default_branch":"main"}
	}`)
	ev, _ := ParseGitHubEvent("push", body)
	if ev.Kind != EventIgnored {
		t.Fatalf("kind=%s", ev.Kind)
	}
}

func TestParseSettlementTag(t *testing.T) {
	body := []byte(`{
		"ref":"settlement-2026Q2",
		"ref_type":"tag",
		"repository":{"html_url":"https://github.com/o/r"}
	}`)
	ev, _ := ParseGitHubEvent("create", body)
	if ev.Kind != EventTagSettlement || ev.TagName != "settlement-2026Q2" {
		t.Fatalf("unexpected: %+v", ev)
	}
}

func TestParsePing(t *testing.T) {
	ev, err := ParseGitHubEvent("ping", []byte(`{"zen":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventIgnored {
		t.Fatalf("ping must be ignored")
	}
}
