// Package covenant implements the ACP Covenant lifecycle and member management.
package covenant

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/crypto"
	"github.com/inkmesh/acp-server/internal/id"
	"github.com/inkmesh/acp-server/internal/tokens"
)

// Valid state transitions.
var transitions = map[string]string{
	"DRAFT":  "OPEN",
	"OPEN":   "ACTIVE",
	"ACTIVE": "LOCKED",
	"LOCKED": "SETTLED",
}

// ValidSpaceTypes enumerates the SpaceTypes recognised by ACR-20 Part 1.
// unit_name is the display label returned to callers so clients can render
// "Ink" vs "Commit" vs "Note" vs "Citation" etc. instead of a generic "token".
// "custom" leaves unit labelling to the covenant owner's configuration.
var ValidSpaceTypes = map[string]string{
	"book":     "Ink",
	"code":     "Commit",
	"music":    "Note",
	"research": "Citation",
	"custom":   "Token",
}

type Covenant struct {
	CovenantID         string    `json:"covenant_id"`
	Version            string    `json:"version"`
	SpaceType          string    `json:"space_type"`
	UnitName           string    `json:"unit_name"` // ACR-20 Part 1: label for tokens in this space (Ink/Commit/Note/…)
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	State              string    `json:"state"`
	OwnerID            string    `json:"owner_id"` // agent_id of owner member; authoritative, not derived
	OwnerSharePct      float64   `json:"owner_share_pct"`
	PlatformSharePct   float64   `json:"platform_share_pct"`
	ContributorPoolPct float64   `json:"contributor_pool_pct"`
	BudgetLimit        int64     `json:"budget_limit"`    // minor units of BudgetCurrency
	BudgetCurrency     string    `json:"budget_currency"` // ISO 4217; all charges must match
	CostWeight         float64   `json:"cost_weight"` // ACR-20 §6: net_delta = tokens_delta - cost_weight × cost_delta
	// ACR-400 Part 1: optional Git Covenant Twin binding. Empty strings mean no twin.
	GitTwinURL        string `json:"git_twin_url,omitempty"`
	GitTwinProvider   string `json:"git_twin_provider,omitempty"`
	GitTwinConfigJSON string `json:"git_twin_config_json,omitempty"`
	// OwnerToken is populated only in the Create response (shown once, never again).
	OwnerToken string    `json:"owner_token,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Member struct {
	CovenantID string `json:"covenant_id"`
	// PlatformID is the plaintext identifier. Per ACR-700 §4, API responses
	// MUST NOT return it — covenant-scoped agent_id is the external handle.
	// The field stays on the struct for internal join/lookup paths but is
	// never serialized to clients.
	PlatformID string    `json:"-"`
	AgentID    string    `json:"agent_id"`
	TierID     string    `json:"tier_id"`
	IsOwner    bool      `json:"is_owner"`
	Status     string    `json:"status"`
	JoinedAt   time.Time `json:"joined_at"`
}

type Service struct {
	db     *sql.DB
	sealer *crypto.Sealer // optional; when set, platform_id writes populate platform_id_enc
}

func New(db *sql.DB) *Service { return &Service{db: db} }

// SetSealer wires an ACR-700 Sealer into the service so that subsequent
// platform_id writes populate the platform_id_enc column. Callers that leave
// the sealer unset still get platform_id_hash (stdlib SHA-256) but platform_id_enc
// remains NULL. Tests that exercise pre-4.5 behavior can omit this call.
func (s *Service) SetSealer(sealer *crypto.Sealer) { s.sealer = sealer }

// sqlExec is satisfied by both *sql.DB and *sql.Tx so upsertPlatformIdentity
// can be called from either context.
type sqlExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// HashPlatformID is the indexable lookup key for a platform_id (ACR-700 §4):
// 64-char lowercase hex SHA-256 of the plaintext. Deterministic; not a
// confidentiality surface. Exported so API handlers can render a
// 12-char prefix in place of plaintext (ACR-700 §4 list_members output).
func HashPlatformID(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// upsertPlatformIdentity writes one platform_identities row with the ACR-700
// hash + enc columns populated. When sealer is nil, platform_id_enc is left
// NULL; plaintext platform_id is still written so the existing covenant_members
// FK continues to resolve until 4.5.7 gates the column drop.
//
// INSERT OR IGNORE keeps the call idempotent: on PK conflict the row already
// exists and (by the time 4.5.4 rolls out) already has hash + enc filled by a
// prior write or by Backfill.
func upsertPlatformIdentity(exec sqlExec, sealer *crypto.Sealer, plaintext, nowRFC3339Nano string) error {
	hash := HashPlatformID(plaintext)
	var enc []byte
	if sealer != nil {
		blob, err := sealer.Seal(hash, "platform_id", []byte(plaintext))
		if err != nil {
			return fmt.Errorf("seal platform_id: %w", err)
		}
		enc = blob
	}
	_, err := exec.Exec(`
		INSERT OR IGNORE INTO platform_identities
			(platform_id, created_at, platform_id_hash, platform_id_enc)
		VALUES (?, ?, ?, ?)`,
		plaintext, nowRFC3339Nano, hash, enc,
	)
	return err
}

// Create builds a new Covenant in DRAFT state and registers the owner as a member.
func (s *Service) Create(title, spaceType, ownerPlatformID string) (*Covenant, *Member, error) {
	if _, ok := ValidSpaceTypes[spaceType]; !ok {
		return nil, nil, fmt.Errorf("invalid space_type %q (allowed: book, code, music, research, custom)", spaceType)
	}
	now := time.Now().UTC()
	covenantID := id.Covenant()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	ownerToken := randomHex(32)
	agentID := id.Agent()
	_, err = tx.Exec(`
		INSERT INTO covenants (covenant_id, title, space_type, state, owner_id, owner_token, created_at, updated_at)
		VALUES (?, ?, ?, 'DRAFT', ?, ?, ?, ?)`,
		covenantID, title, spaceType, agentID, ownerToken, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create covenant: %w", err)
	}

	if err := upsertPlatformIdentity(tx, s.sealer, ownerPlatformID, now.Format(time.RFC3339Nano)); err != nil {
		return nil, nil, err
	}

	_, err = tx.Exec(`
		INSERT INTO covenant_members (covenant_id, platform_id, agent_id, is_owner, status, joined_at)
		VALUES (?, ?, ?, 1, 'active', ?)`,
		covenantID, ownerPlatformID, agentID, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	cov := &Covenant{
		CovenantID:         covenantID,
		Version:            "ACP@1.0",
		SpaceType:          spaceType,
		UnitName:           ValidSpaceTypes[spaceType],
		Title:              title,
		State:              "DRAFT",
		OwnerID:            agentID,
		OwnerSharePct:      30,
		PlatformSharePct:   0,
		ContributorPoolPct: 70,
		BudgetCurrency:     "USD", // matches schema default
		CostWeight:         1.0,   // matches schema default
		OwnerToken:         ownerToken, // shown once in Create response
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	mem := &Member{
		CovenantID: covenantID,
		PlatformID: ownerPlatformID,
		AgentID:    agentID,
		IsOwner:    true,
		Status:     "active",
		JoinedAt:   now,
	}
	return cov, mem, nil
}

// AddTier registers an AccessTier for an OPEN covenant.
func (s *Service) AddTier(covenantID, tierID, displayName string, tokenMultiplier float64, maxSlots *int) error {
	var maxSlotsVal interface{}
	if maxSlots != nil {
		maxSlotsVal = *maxSlots
	}
	_, err := s.db.Exec(`
		INSERT INTO access_tiers (tier_id, covenant_id, display_name, token_multiplier, max_slots)
		VALUES (?, ?, ?, ?, ?)`,
		tierID, covenantID, displayName, tokenMultiplier, maxSlotsVal,
	)
	return err
}

// Transition advances covenant state along the valid transition path.
func (s *Service) Transition(covenantID, targetState string) (*Covenant, error) {
	cov, err := s.Get(covenantID)
	if err != nil {
		return nil, err
	}
	expected, ok := transitions[cov.State]
	if !ok || expected != targetState {
		return nil, fmt.Errorf("invalid transition %s → %s", cov.State, targetState)
	}
	if cov.State == "DRAFT" {
		// Must have at least one tier before OPEN
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM access_tiers WHERE covenant_id=?`, covenantID).Scan(&n)
		if n == 0 {
			return nil, errors.New("covenant must have at least one access tier before opening")
		}
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(`UPDATE covenants SET state=?, updated_at=? WHERE covenant_id=?`,
		targetState, now.Format(time.RFC3339Nano), covenantID)
	if err != nil {
		return nil, err
	}
	// ACR-20 Part 5: ACTIVE → LOCKED freezes the token ledger into a hashed
	// per-agent snapshot. Runs after the state flip so CaptureSnapshot sees
	// the new state in any concurrent audit.
	if cov.State == "ACTIVE" && targetState == "LOCKED" {
		if _, err := tokens.CaptureSnapshot(s.db, covenantID); err != nil {
			return nil, fmt.Errorf("capture snapshot: %w", err)
		}
	}
	cov.State = targetState
	cov.UpdatedAt = now
	return cov, nil
}

// Join adds a new member to an OPEN covenant.
func (s *Service) Join(covenantID, platformID, tierID string) (*Member, error) {
	cov, err := s.Get(covenantID)
	if err != nil {
		return nil, err
	}
	if cov.State != "OPEN" {
		return nil, fmt.Errorf("covenant is not OPEN (state=%s)", cov.State)
	}

	var multiplier float64
	var maxSlots *int
	var ms sql.NullInt64
	err = s.db.QueryRow(`SELECT token_multiplier, max_slots FROM access_tiers WHERE covenant_id=? AND tier_id=?`,
		covenantID, tierID).Scan(&multiplier, &ms)
	if err != nil {
		return nil, fmt.Errorf("tier %q not found: %w", tierID, err)
	}
	if ms.Valid {
		n := int(ms.Int64)
		maxSlots = &n
	}
	_ = multiplier

	if maxSlots != nil {
		var count int
		s.db.QueryRow(`SELECT COUNT(*) FROM covenant_members WHERE covenant_id=? AND tier_id=?`,
			covenantID, tierID).Scan(&count)
		if count >= *maxSlots {
			return nil, fmt.Errorf("tier %q is full (%d/%d)", tierID, count, *maxSlots)
		}
	}

	if err := upsertPlatformIdentity(s.db, s.sealer, platformID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	agentID := id.Agent()
	_, err = s.db.Exec(`
		INSERT INTO covenant_members (covenant_id, platform_id, agent_id, tier_id, is_owner, status, joined_at)
		VALUES (?, ?, ?, ?, 0, 'pending', ?)`,
		covenantID, platformID, agentID, tierID, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("join covenant: %w", err)
	}

	return &Member{
		CovenantID: covenantID,
		PlatformID: platformID,
		AgentID:    agentID,
		TierID:     tierID,
		Status:     "pending",
		JoinedAt:   now,
	}, nil
}

// Get returns a Covenant by ID.
func (s *Service) Get(covenantID string) (*Covenant, error) {
	c := &Covenant{}
	var createdStr, updatedStr string
	err := s.db.QueryRow(`
		SELECT covenant_id, version, space_type, title, description, state,
		       owner_id, owner_share_pct, platform_share_pct, contributor_pool_pct,
		       budget_limit, budget_currency, cost_weight,
		       git_twin_url, git_twin_provider, git_twin_config_json,
		       created_at, updated_at
		FROM covenants WHERE covenant_id=?`, covenantID,
	).Scan(&c.CovenantID, &c.Version, &c.SpaceType, &c.Title, &c.Description, &c.State,
		&c.OwnerID, &c.OwnerSharePct, &c.PlatformSharePct, &c.ContributorPoolPct,
		&c.BudgetLimit, &c.BudgetCurrency, &c.CostWeight,
		&c.GitTwinURL, &c.GitTwinProvider, &c.GitTwinConfigJSON,
		&createdStr, &updatedStr)
	if err != nil {
		return nil, fmt.Errorf("covenant %q: %w", covenantID, err)
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	if name, ok := ValidSpaceTypes[c.SpaceType]; ok {
		c.UnitName = name
	} else {
		c.UnitName = "Token"
	}
	return c, nil
}

// SetGitTwin binds a covenant to a git repo Digital Twin (ACR-400 Part 1).
// Allowed only while state=DRAFT; after OPEN the binding is immutable so
// participants who joined cannot be blindsided by a late-stage rebinding.
// Pass empty strings to clear the binding (only while DRAFT).
func (s *Service) SetGitTwin(covenantID, url, provider, configJSON string) error {
	cov, err := s.Get(covenantID)
	if err != nil {
		return err
	}
	if cov.State != "DRAFT" {
		return fmt.Errorf("git twin can only be set while DRAFT (current state: %s)", cov.State)
	}
	if url != "" {
		switch provider {
		case "github", "gitlab", "gitea", "local-hook":
		default:
			return fmt.Errorf("invalid git_twin_provider %q (allowed: github, gitlab, gitea, local-hook)", provider)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`
		UPDATE covenants SET git_twin_url=?, git_twin_provider=?, git_twin_config_json=?, updated_at=?
		WHERE covenant_id=?`,
		url, provider, configJSON, now, covenantID)
	return err
}

// FindByGitTwinURL returns the covenant_ids currently bound to the given repo URL.
// Used by the git bridge (ACR-400 Part 7) to route a webhook to the right covenant.
// A single repo may be twinned by multiple covenants (ACR-400 Part 1) — caller decides.
func (s *Service) FindByGitTwinURL(url string) ([]string, error) {
	if url == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT covenant_id FROM covenants WHERE git_twin_url=?`, url)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// FindMemberByPlatformID locates the active member whose platform_id matches
// in a given covenant. ACR-400 Part 3: the bridge uses this to map a GitHub
// author to the ACP agent_id. Returns ErrNoMember when no match exists so
// callers can distinguish "unmapped contribution" from a real lookup error.
var ErrNoMember = errors.New("no active member with this platform_id")

func (s *Service) FindMemberByPlatformID(covenantID, platformID string) (*Member, error) {
	m := &Member{}
	var joinedStr string
	var tierID sql.NullString
	err := s.db.QueryRow(`
		SELECT covenant_id, platform_id, agent_id, COALESCE(tier_id,''), is_owner, status, joined_at
		FROM covenant_members WHERE covenant_id=? AND platform_id=? AND status='active'`,
		covenantID, platformID,
	).Scan(&m.CovenantID, &m.PlatformID, &m.AgentID, &tierID, &m.IsOwner, &m.Status, &joinedStr)
	if err == sql.ErrNoRows {
		return nil, ErrNoMember
	}
	if err != nil {
		return nil, err
	}
	_ = tierID
	m.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedStr)
	return m, nil
}

// GetMember returns a CovenantMember by covenant + agent.
func (s *Service) GetMember(covenantID, agentID string) (*Member, error) {
	m := &Member{}
	var joinedStr string
	var tierID sql.NullString
	err := s.db.QueryRow(`
		SELECT covenant_id, platform_id, agent_id, COALESCE(tier_id,''), is_owner, status, joined_at
		FROM covenant_members WHERE covenant_id=? AND agent_id=?`,
		covenantID, agentID,
	).Scan(&m.CovenantID, &m.PlatformID, &m.AgentID, &tierID, &m.IsOwner, &m.Status, &joinedStr)
	if err != nil {
		return nil, fmt.Errorf("member %q in %q: %w", agentID, covenantID, err)
	}
	_ = tierID
	m.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedStr)
	return m, nil
}

// TierMultiplier returns the token_multiplier for a tier (defaults to 1.0).
func (s *Service) TierMultiplier(covenantID, tierID string) float64 {
	var m float64
	err := s.db.QueryRow(`SELECT token_multiplier FROM access_tiers WHERE covenant_id=? AND tier_id=?`,
		covenantID, tierID).Scan(&m)
	if err != nil {
		return 1.0
	}
	return m
}

// State returns the current state of a covenant.
func (s *Service) State(covenantID string) (string, error) {
	cov, err := s.Get(covenantID)
	if err != nil {
		return "", err
	}
	return cov.State, nil
}

// GetOwnerAgentID returns the agent_id of the covenant owner.
// Reads covenants.owner_id directly; falls back to the is_owner=1 lookup for
// pre-migration rows that somehow bypassed the backfill.
func (s *Service) GetOwnerAgentID(covenantID string) (string, error) {
	var agentID string
	err := s.db.QueryRow(`SELECT owner_id FROM covenants WHERE covenant_id=?`, covenantID).Scan(&agentID)
	if err != nil {
		return "", fmt.Errorf("owner agent for covenant %q: %w", covenantID, err)
	}
	if agentID != "" {
		return agentID, nil
	}
	err = s.db.QueryRow(
		`SELECT agent_id FROM covenant_members WHERE covenant_id=? AND is_owner=1`,
		covenantID,
	).Scan(&agentID)
	if err != nil {
		return "", fmt.Errorf("owner agent for covenant %q: %w", covenantID, err)
	}
	return agentID, nil
}

// GetOwnerToken returns the owner_token for a covenant (used to validate X-Owner-Token headers).
func (s *Service) GetOwnerToken(covenantID string) (string, error) {
	var token string
	err := s.db.QueryRow(`SELECT owner_token FROM covenants WHERE covenant_id=?`, covenantID).Scan(&token)
	if err != nil {
		return "", fmt.Errorf("covenant %q: %w", covenantID, err)
	}
	return token, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
