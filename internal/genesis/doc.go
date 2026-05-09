// Package genesis is RESERVED for the Phase 6 implementation of
// ACR-600 (Genesis Migration). It is intentionally empty.
//
// DO NOT IMPLEMENT until a specific OSS project has formally
// requested onboarding their pre-ACP git history into a Covenant.
//
// The roadmap names OpenClaw as the candidate but no commitment
// exists yet. Building this without a concrete first adopter
// produces a migration tool nobody runs.
//
// When the trigger arrives:
//
//  1. ACR-600 v0.1 (the spec) is reviewed against the candidate
//     project's actual git history shape.
//  2. Compliance review on Howey Test implications of GT distribution
//     (mandatory before any first import involving real money).
//  3. genesis_imports + genesis_allocations tables created (schema
//     in ACR-600 v0.1).
//  4. CLI tool acp-genesis-import implemented in cmd/acp-genesis-import/.
//  5. Pilot import on the first adopter, results captured for v0.2.
//
// Genesis Token (GT) is non-transferable, non-tradeable. Confirming
// that property survives implementation is the first compliance gate
// — it's the difference between a contribution-share record and a
// security under U.S. law.
//
// Reservation rationale: same as internal/escrow/.
package genesis
