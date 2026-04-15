// Package covenant implements the ACP Covenant lifecycle and member management.
package covenant

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

// Valid state transitions.
var transitions = map[string]string{
	"DRAFT":  "OPEN",
	"OPEN":   "ACTIVE",
	"ACTIVE": "LOCKED",
	"LOCKED": "SETTLED",
}

type Covenant struct {
	CovenantID         string    `json:"covenant_id"`
	Version            string    `json:"version"`
	SpaceType          string    `json:"space_type"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	State              string    `json:"state"`
	OwnerSharePct      float64   `json:"owner_share_pct"`
	PlatformSharePct   float64   `json:"platform_share_pct"`
	ContributorPoolPct float64   `json:"contributor_pool_pct"`
	BudgetLimit        float64   `json:"budget_limit"`
	// OwnerToken is populated only in the Create response (shown once, never again).
	OwnerToken string    `json:"owner_token,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Member struct {
	CovenantID string    `json:"covenant_id"`
	PlatformID string    `json:"platform_id"`
	AgentID    string    `json:"agent_id"`
	TierID     string    `json:"tier_id"`
	IsOwner    bool      `json:"is_owner"`
	Status     string    `json:"status"`
	JoinedAt   time.Time `json:"joined_at"`
}

type Service struct{ db *sql.DB }

func New(db *sql.DB) *Service { return &Service{db: db} }

// Create builds a new Covenant in DRAFT state and registers the owner as a member.
func (s *Service) Create(title, spaceType, ownerPlatformID string) (*Covenant, *Member, error) {
	now := time.Now().UTC()
	covenantID := id.Covenant()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	ownerToken := randomHex(32)
	_, err = tx.Exec(`
		INSERT INTO covenants (covenant_id, title, space_type, state, owner_token, created_at, updated_at)
		VALUES (?, ?, ?, 'DRAFT', ?, ?, ?)`,
		covenantID, title, spaceType, ownerToken, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create covenant: %w", err)
	}

	_, err = tx.Exec(`
		INSERT OR IGNORE INTO platform_identities (platform_id, created_at)
		VALUES (?, ?)`,
		ownerPlatformID, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, nil, err
	}

	agentID := id.Agent()
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
		Title:              title,
		State:              "DRAFT",
		OwnerSharePct:      30,
		PlatformSharePct:   0,
		ContributorPoolPct: 70,
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

	_, err = s.db.Exec(`INSERT OR IGNORE INTO platform_identities (platform_id, created_at) VALUES (?, ?)`,
		platformID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
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
		       owner_share_pct, platform_share_pct, contributor_pool_pct,
		       budget_limit, created_at, updated_at
		FROM covenants WHERE covenant_id=?`, covenantID,
	).Scan(&c.CovenantID, &c.Version, &c.SpaceType, &c.Title, &c.Description, &c.State,
		&c.OwnerSharePct, &c.PlatformSharePct, &c.ContributorPoolPct,
		&c.BudgetLimit, &createdStr, &updatedStr)
	if err != nil {
		return nil, fmt.Errorf("covenant %q: %w", covenantID, err)
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return c, nil
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

// GetOwnerAgentID returns the agent_id of the covenant owner member.
func (s *Service) GetOwnerAgentID(covenantID string) (string, error) {
	var agentID string
	err := s.db.QueryRow(
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
