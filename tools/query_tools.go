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
