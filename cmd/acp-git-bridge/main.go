// Command acp-git-bridge is the ACR-400 Git Covenant Twin bridge.
//
// Flow: GitHub webhook → HMAC verify → parse event → look up covenants for
// the repo via acp-server → forward pull_request.merged events as
// /git-twin/merge calls. In parallel, a goroutine polls acp-server for
// pending Git Anchors (ACR-400 Part 5), writes them as git notes on
// refs/notes/acp-anchors in the twinned repo, and acks back to the server
// with the resulting commit SHA. Other events (PR open/closed-unmerged,
// push, force-push, settlement tag) are logged but not yet mapped —
// deferred until their server-side counterparts land.
//
// Env:
//
//	ACP_BRIDGE_ADDR                 listen addr, default :8090
//	ACP_BRIDGE_GITHUB_SECRET        webhook HMAC secret configured on GitHub side
//	ACP_BRIDGE_SECRET               shared secret with acp-server for /git-twin/* auth
//	ACP_SERVER_URL                  acp-server base URL, default http://127.0.0.1:8080
//	ACP_BRIDGE_ANCHOR_POLL_SECONDS  poll cadence for /git-twin/anchors/pending, default 30
//	ACP_BRIDGE_ANCHOR_WRITER        "1" enables the writer (default), "0" disables for handler-only deploys
//	ACP_BRIDGE_ANCHOR_CACHE_DIR     override for per-repo clone cache (defaults to os.UserCacheDir/acp-git-bridge/repos)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/inkmesh/acp-server/internal/gittwin"
)

func main() {
	addr := envOrDefault("ACP_BRIDGE_ADDR", ":8090")
	ghSecret := os.Getenv("ACP_BRIDGE_GITHUB_SECRET")
	if ghSecret == "" {
		log.Fatal("ACP_BRIDGE_GITHUB_SECRET is required")
	}
	bridgeSecret := os.Getenv("ACP_BRIDGE_SECRET")
	if bridgeSecret == "" {
		log.Fatal("ACP_BRIDGE_SECRET is required (must match acp-server's ACP_BRIDGE_SECRET)")
	}
	serverURL := envOrDefault("ACP_SERVER_URL", "http://127.0.0.1:8080")

	h := &githubHandler{
		githubSecret: ghSecret,
		bridgeSecret: bridgeSecret,
		serverURL:    serverURL,
		client:       &http.Client{Timeout: 10 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.Handle("POST /webhook/github", h)

	// Anchor writer: opt-out, but only really opt-out when an operator sets
	// ACP_BRIDGE_ANCHOR_WRITER=0. Cache dir is lazily created by the writer
	// on the first job.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if envOrDefault("ACP_BRIDGE_ANCHOR_WRITER", "1") != "0" {
		cacheDir := envOrDefault("ACP_BRIDGE_ANCHOR_CACHE_DIR", gittwin.DefaultCacheDir())
		poll := envDurationSeconds("ACP_BRIDGE_ANCHOR_POLL_SECONDS", 30)
		writer := &gittwin.AnchorWriter{CacheDir: cacheDir, Runner: gittwin.NewExecGitRunner()}
		go runAnchorLoop(ctx, h, writer, poll)
		log.Printf("acp-git-bridge anchor writer enabled: cache=%s poll=%s", cacheDir, poll)
	} else {
		log.Printf("acp-git-bridge anchor writer disabled via ACP_BRIDGE_ANCHOR_WRITER=0")
	}

	log.Printf("acp-git-bridge listening on %s (→ %s)", addr, serverURL)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		// Best-effort graceful shutdown so an in-flight webhook doesn't get
		// cut mid-request on SIGTERM.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// runAnchorLoop polls acp-server for pending anchors and drains them through
// the writer. Errors never kill the loop: a bad repo, transient network
// error, or push failure should leave the anchor pending so the next tick
// retries. We log at warning level so operators see repeated failures.
func runAnchorLoop(ctx context.Context, h *githubHandler, writer *gittwin.AnchorWriter, poll time.Duration) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	// Kick immediately rather than waiting a full tick on startup — lets
	// operators unblock a backlog by restarting the bridge.
	h.drainAnchors(ctx, writer)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.drainAnchors(ctx, writer)
		}
	}
}

type githubHandler struct {
	githubSecret string
	bridgeSecret string
	serverURL    string
	client       *http.Client
}

func (h *githubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := gittwin.VerifyGitHubSignature(body, r.Header.Get("X-Hub-Signature-256"), h.githubSecret); err != nil {
		log.Printf("reject: signature verify: %v", err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	eventHeader := r.Header.Get("X-GitHub-Event")
	ev, err := gittwin.ParseGitHubEvent(eventHeader, body)
	if err != nil {
		log.Printf("reject: parse %s: %v", eventHeader, err)
		http.Error(w, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	logEvent(ev)

	// Ignored events (ping, feature-branch non-force push, non-settlement tag)
	// land here with no further server call.
	if ev.Kind == gittwin.EventIgnored {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "accepted (ignored)\n")
		return
	}

	covenantIDs, err := h.findCovenantsForRepo(ev.RepoURL)
	if err != nil {
		log.Printf("find covenants for %s: %v", ev.RepoURL, err)
		http.Error(w, "covenant lookup failed", http.StatusBadGateway)
		return
	}
	if len(covenantIDs) == 0 {
		log.Printf("no covenant bound to repo %s; ignoring %s", ev.RepoURL, ev.Kind)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "accepted (no twin)\n")
		return
	}

	type forwardResult struct {
		CovenantID string         `json:"covenant_id"`
		Status     int            `json:"status"`
		Response   map[string]any `json:"response,omitempty"`
		Error      string         `json:"error,omitempty"`
	}
	var results []forwardResult

	switch ev.Kind {
	case gittwin.EventPullRequestMerged:
		// Merge is the only ledger-mutating path: propose_passage + approve_draft.
		unitCount := ev.PRAdditions - ev.PRDeletions
		if unitCount <= 0 {
			unitCount = 1
		}
		for _, cid := range covenantIDs {
			status, resp, err := h.postMerge(cid, ev, unitCount)
			fr := forwardResult{CovenantID: cid, Status: status, Response: resp}
			if err != nil {
				fr.Error = err.Error()
				log.Printf("forward merge %s failed: %v", cid, err)
			}
			results = append(results, fr)
		}
	default:
		// All other recognized kinds (push.forced, push.protected,
		// pull_request.opened, pull_request.rejected, tag.settlement) land as
		// audit-only entries. record_git_twin_event does not touch the ledger
		// but preserves the twin history for verifiers.
		for _, cid := range covenantIDs {
			status, resp, err := h.postEvent(cid, ev)
			fr := forwardResult{CovenantID: cid, Status: status, Response: resp}
			if err != nil {
				fr.Error = err.Error()
				log.Printf("forward event %s to %s failed: %v", ev.Kind, cid, err)
			}
			results = append(results, fr)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"forwarded": results,
		"event":     ev.Kind,
	})
}

func (h *githubHandler) findCovenantsForRepo(repoURL string) ([]string, error) {
	if repoURL == "" {
		return nil, nil
	}
	u, err := url.Parse(h.serverURL + "/git-twin/covenants")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("repo_url", repoURL)
	u.RawQuery = q.Encode()

	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Set("X-Bridge-Secret", h.bridgeSecret)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("acp-server %d: %s", resp.StatusCode, string(b))
	}
	var body struct {
		CovenantIDs []string `json:"covenant_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.CovenantIDs, nil
}

func (h *githubHandler) postMerge(covenantID string, ev gittwin.ParsedEvent, unitCount int) (int, map[string]any, error) {
	body := map[string]any{
		"covenant_id":        covenantID,
		"author_platform_id": gittwin.PlatformIDFromGitHubLogin(ev.PRAuthor),
		"draft_ref":          ev.PRURL,
		"unit_count":         unitCount,
		"acceptance_ratio":   1.0,
		"summary":            ev.PRTitle,
		"content_hash":       ev.CommitHead,
	}
	raw, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", h.serverURL+"/git-twin/merge", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Secret", h.bridgeSecret)

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(buf, &decoded)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, decoded, fmt.Errorf("acp-server %d: %s", resp.StatusCode, string(buf))
	}
	return resp.StatusCode, decoded, nil
}

// postEvent forwards a non-merge git event as an audit-only record. The
// server side resolves actor_platform_id → agent or falls back to the
// covenant owner, so we pass whatever we know without probing locally.
func (h *githubHandler) postEvent(covenantID string, ev gittwin.ParsedEvent) (int, map[string]any, error) {
	body := map[string]any{
		"covenant_id":       covenantID,
		"actor_platform_id": eventActorPlatformID(ev),
		"event_kind":        string(ev.Kind),
		"ref":               eventRef(ev),
		"commit_head":       ev.CommitHead,
		"summary":           eventSummary(ev),
		"source_ref":        eventSourceRef(ev),
	}
	raw, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", h.serverURL+"/git-twin/event", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Secret", h.bridgeSecret)

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(buf, &decoded)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, decoded, fmt.Errorf("acp-server %d: %s", resp.StatusCode, string(buf))
	}
	return resp.StatusCode, decoded, nil
}

// eventActorPlatformID picks the best-known GitHub login for the event.
// PRs use the PR author; pushes use the first commit author; tags we leave
// blank (GitHub create payload has no author field) and let the server fall
// back to the owner.
func eventActorPlatformID(ev gittwin.ParsedEvent) string {
	switch {
	case ev.PRAuthor != "":
		return gittwin.PlatformIDFromGitHubLogin(ev.PRAuthor)
	case len(ev.PushAuthors) > 0:
		return gittwin.PlatformIDFromGitHubLogin(ev.PushAuthors[0])
	default:
		return ""
	}
}

func eventRef(ev gittwin.ParsedEvent) string {
	if ev.PushRef != "" {
		return ev.PushRef
	}
	if ev.PRBaseRef != "" {
		return "refs/heads/" + ev.PRBaseRef
	}
	return ""
}

func eventSourceRef(ev gittwin.ParsedEvent) string {
	if ev.PRHeadRef != "" {
		return "refs/heads/" + ev.PRHeadRef
	}
	if ev.TagName != "" {
		return ev.TagName
	}
	return ""
}

func eventSummary(ev gittwin.ParsedEvent) string {
	switch ev.Kind {
	case gittwin.EventPullRequestOpened, gittwin.EventPullRequestRejected:
		return ev.PRTitle
	case gittwin.EventPushForced:
		return "force-push to " + ev.PushRef
	case gittwin.EventPushProtected:
		return "push to " + ev.PushRef
	case gittwin.EventTagSettlement:
		return "settlement tag " + ev.TagName
	default:
		return ""
	}
}

func logEvent(ev gittwin.ParsedEvent) {
	summary := map[string]any{
		"kind":      ev.Kind,
		"repo":      ev.RepoURL,
		"pr":        ev.PRNumber,
		"pr_author": ev.PRAuthor,
		"additions": ev.PRAdditions,
		"deletions": ev.PRDeletions,
		"forced":    ev.PushForced,
		"push_ref":  ev.PushRef,
		"tag":       ev.TagName,
	}
	js, _ := json.Marshal(summary)
	log.Printf("github event: %s", js)
}

func envOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDurationSeconds(k string, defSeconds int) time.Duration {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defSeconds) * time.Second
}

// drainAnchors lists every pending anchor on the server, writes each one as
// a git note, and acks the commit SHA back. A bad individual anchor does
// not abort the batch — we loop so the healthy ones still land.
func (h *githubHandler) drainAnchors(ctx context.Context, writer *gittwin.AnchorWriter) {
	anchors, err := h.listPendingAnchors(ctx)
	if err != nil {
		log.Printf("list pending anchors: %v", err)
		return
	}
	for _, a := range anchors {
		sha, err := writer.Write(ctx, gittwin.AnchorJob{
			AnchorID: a.AnchorID,
			RepoURL:  a.RepoURL,
			NoteBody: a.NoteBody,
		})
		if err != nil {
			log.Printf("write anchor %s (%s): %v", a.AnchorID, a.RepoURL, err)
			continue
		}
		if err := h.ackAnchor(ctx, a.AnchorID, sha); err != nil {
			log.Printf("ack anchor %s sha=%s: %v", a.AnchorID, sha, err)
			continue
		}
		log.Printf("anchor %s written: repo=%s sha=%s", a.AnchorID, a.RepoURL, sha)
	}
}

type pendingAnchor struct {
	AnchorID           string `json:"anchor_id"`
	CovenantID         string `json:"covenant_id"`
	SettlementOutputID string `json:"settlement_output_id"`
	RepoURL            string `json:"repo_url"`
	NoteBody           string `json:"note_body"`
}

func (h *githubHandler) listPendingAnchors(ctx context.Context) ([]pendingAnchor, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", h.serverURL+"/git-twin/anchors/pending", nil)
	req.Header.Set("X-Bridge-Secret", h.bridgeSecret)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("acp-server %d: %s", resp.StatusCode, string(b))
	}
	var body struct {
		Anchors []pendingAnchor `json:"anchors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Anchors, nil
}

func (h *githubHandler) ackAnchor(ctx context.Context, anchorID, commitSHA string) error {
	payload, _ := json.Marshal(map[string]string{"written_commit_sha": commitSHA})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		h.serverURL+"/git-twin/anchors/"+url.PathEscape(anchorID)+"/ack",
		bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bridge-Secret", h.bridgeSecret)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("acp-server %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
