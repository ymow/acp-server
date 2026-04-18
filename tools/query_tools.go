// Package tools — Phase 2 query tool stubs.
// These types satisfy the Tool interface metadata contract (ToolName / ToolType).
// Their query logic runs directly in api.go handlers via handleGetTokenBalance
// and handleListMembers, bypassing the execution engine (no side effects, no budget).
package tools

// GetTokenBalance is a metadata stub for the get_token_balance query tool.
type GetTokenBalance struct{}

func (t *GetTokenBalance) ToolName() string { return "get_token_balance" }
func (t *GetTokenBalance) ToolType() string { return "query" }

// ListMembers is a metadata stub for the list_members query tool.
type ListMembers struct{}

func (t *ListMembers) ToolName() string { return "list_members" }
func (t *ListMembers) ToolType() string { return "query" }

// GetTokenHistory is a metadata stub for the get_token_history query tool
// (ACR-20 Part 7). Query runs in api.go handleGetTokenHistory.
type GetTokenHistory struct{}

func (t *GetTokenHistory) ToolName() string { return "get_token_history" }
func (t *GetTokenHistory) ToolType() string { return "query" }

// GetConcentrationStatus is a metadata stub for the owner-only Layer 5 query
// that surfaces token concentration across covenant members (ACR-20 Part 4).
// Query runs in api.go handleGetConcentrationStatus.
type GetConcentrationStatus struct{}

func (t *GetConcentrationStatus) ToolName() string { return "get_concentration_status" }
func (t *GetConcentrationStatus) ToolType() string { return "query" }
