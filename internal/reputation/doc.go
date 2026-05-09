// Package reputation is RESERVED for the Phase 5 implementation of
// ACR-200 (Cross-Covenant Reputation). It is intentionally empty.
//
// DO NOT IMPLEMENT until BOTH gates clear:
//
//  1. Phase 7.A (escrow) ships and is operationally proven.
//  2. ≥30 settled Covenants on real money have produced data the
//     ACR-200 ARS formula can be tuned against.
//
// The roadmap is explicit about the second gate: reputation built
// before real-money settlement is noise. We don't tune a formula
// against synthetic data and pretend it generalises.
//
// What MUST happen during Phase 7.A implementation (sister package
// internal/escrow/) is that the schema preserves enough context per
// passage and per settlement so that, when this package's eventual
// implementation runs, it has the data it needs:
//
//   - per-passage acceptance_ratio
//   - completion timestamps
//   - agent_id (already present)
//   - tier_multiplier (already present)
//   - escrow tx_hash (after Phase 7.A)
//
// See ACR-200 v0.1 §"Path to ratification" for the ordered work.
//
// When ratification completes:
//
//  1. ACR-200 v0.2 written from observed real-tx data; ARS formula
//     possibly modified per R-1..R-8 outcomes.
//  2. agent_reputation table created (schema in ACR-200 v0.1).
//  3. Recompute job (background) implemented here.
//  4. cross-server federated query endpoint added (separate ACR-210
//     specifies the federation protocol).
//
// Reservation rationale: same as internal/escrow/.
package reputation
