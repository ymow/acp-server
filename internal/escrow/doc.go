// Package escrow is RESERVED for the Phase 7.A implementation of
// ACR-500 (Covenant Escrow Standard). It is intentionally empty.
//
// DO NOT IMPLEMENT until ACR-500 v0.3 is RATIFIED.
//
// The ten ACR-500 v0.1 decisions in:
//
//	inkmesh/Agent Covenant Protocol_ACP/ACR-500_Decisions_v0.1.md
//
// MUST be resolved by the working group before any code lands here.
// Per docs/PHASE-7A-DECISIONS.md anti-patterns: speculative
// implementation against PROVISIONAL v0.2 is the failure mode this
// reservation exists to prevent.
//
// When ratification completes:
//
//  1. Update ACR-500 to v0.3 with explicit RATIFIED markers per
//     decision.
//  2. Add the schema work documented in ACR-500 v0.2 Section
//     "Schema additions".
//  3. Implement the tool surface (lock_escrow, top_up_escrow,
//     release_to_claimants, refund_to_owner, get_escrow_status).
//  4. Wire the chosen custody adapter (per C-7 outcome — Profile A
//     audited Solidity, Profile B 2-of-2 multisig, or Profile C
//     vendor WaaS).
//  5. Update internal/settlement/ to call this package on
//     confirm_settlement_output for Covenants opted into escrow.
//
// This package's existence reserves the import path so a future
// contributor doesn't accidentally write `internal/escrowing/` or
// `internal/escrow_v1/` and split the implementation across multiple
// namespaces.
package escrow
