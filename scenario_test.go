package main_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/tokens"
	"github.com/inkmesh/acp-server/tools"
)

// TestE2EScenario 覆蓋完整 E2E 情境：
//   - 正常流程：建立 Covenant → 兩個 Agent 加入 → 貢獻 → 結算
//   - 預算超額邊界案例
//   - 重複申請邊界案例
//   - 斷言 SettlementOutput A+B = 100%
func TestE2EScenario(t *testing.T) {
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 初始化：建立測試用資料庫
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	conn, err := db.Open(t.TempDir() + "/e2e_scenario.db")
	if err != nil {
		t.Fatalf("無法開啟資料庫: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 1：Owner 建立 Covenant（業務語義：書籍專案初始化）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	cov, ownerMem, err := covSvc.Create("科幻小說聯合創作", "book", "pid_owner_alice")
	mustOK(t, err, "建立 Covenant")
	if cov.State != "DRAFT" {
		t.Fatalf("預期 DRAFT 狀態，實際為 %s", cov.State)
	}
	t.Logf("【建立成功】 Covenant ID = %s, Owner Agent = %s", cov.CovenantID, ownerMem.AgentID)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 1b：AC-1 — configure_token_rules（DRAFT 狀態下，activate_covenant 前）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	cfgReceipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_cfg",
		&tools.ConfigureTokenRules{}, map[string]any{
			"rules": map[string]any{
				"proposal_cost": 10,
				"base_rate":     1.0,
			},
		})
	mustOK(t, err, "configure_token_rules")
	t.Logf("【AC-1】 configure_token_rules 成功 → audit_log_id=%s", cfgReceipt.ReceiptID)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 2：Owner 設定存取層級（業務語義：定義作者分潤比例）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	mustOK(t, covSvc.AddTier(cov.CovenantID, "author", "作者", 1.0, nil), "新增作者層級")
	mustOK(t, covSvc.AddTier(cov.CovenantID, "senior", "資深作者", 1.5, intPtr(3)), "新增資深作者層級（限3名）")
	t.Log("【層級設定】 author (1.0x), senior (1.5x, max 3)")

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 3：Owner 開放加入（DRAFT → OPEN）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	cov, err = covSvc.Transition(cov.CovenantID, "OPEN")
	mustOK(t, err, "轉換到 OPEN")
	t.Log("【狀態轉換】 DRAFT → OPEN")

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 4：兩位 Agent 加入（業務語義：兩位作者報名參與）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	agentA, err := covSvc.Join(cov.CovenantID, "pid_writer_bob", "author")
	mustOK(t, err, "Agent A 加入")
	t.Logf("【加入成功】 Agent A = %s (作者層級)", agentA.AgentID)

	agentB, err := covSvc.Join(cov.CovenantID, "pid_writer_carol", "senior")
	mustOK(t, err, "Agent B 加入")
	t.Logf("【加入成功】 Agent B = %s (資深作者層級，1.5x 加成)", agentB.AgentID)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 4b：AC-2 — approve_agent × 2（Owner 手動核准兩位 Agent）
	// 先將 agents 設為 pending（模擬需要審批的入會流程）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	_, err = conn.Exec(`UPDATE covenant_members SET status='pending' WHERE covenant_id=? AND agent_id=?`,
		cov.CovenantID, agentA.AgentID)
	mustOK(t, err, "設定 Agent A 為 pending")
	_, err = conn.Exec(`UPDATE covenant_members SET status='pending' WHERE covenant_id=? AND agent_id=?`,
		cov.CovenantID, agentB.AgentID)
	mustOK(t, err, "設定 Agent B 為 pending")

	aaReceipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentA.AgentID})
	mustOK(t, err, "approve_agent Agent A")
	if aaReceipt.Extra["status"] != "active" {
		t.Errorf("【失敗】 approve_agent Agent A 應回傳 active，實際=%v", aaReceipt.Extra["status"])
	}
	t.Logf("【AC-2】 approve_agent Agent A → status=%v, audit_log_id=%s", aaReceipt.Extra["status"], aaReceipt.ReceiptID)

	abReceipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentB.AgentID})
	mustOK(t, err, "approve_agent Agent B")
	if abReceipt.Extra["status"] != "active" {
		t.Errorf("【失敗】 approve_agent Agent B 應回傳 active，實際=%v", abReceipt.Extra["status"])
	}
	t.Logf("【AC-2】 approve_agent Agent B → status=%v, audit_log_id=%s", abReceipt.Extra["status"], abReceipt.ReceiptID)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 5：啟動創作（OPEN → ACTIVE）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	cov, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	mustOK(t, err, "轉換到 ACTIVE")
	t.Log("【狀態轉換】 OPEN → ACTIVE（開始接受貢獻）")

	// 設定預算（業務語義：本專案總預算 $200）
	mustOK(t, budget.EnsureCounter(conn, cov.CovenantID, 200.0, "USD"), "設定預算")
	t.Log("【預算設定】 $200.00")

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 6：Agent A 提交貢獻（業務語義：投稿 1000 字章節）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	receiptA1, err := engine.Run(cov.CovenantID, agentA.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	mustOK(t, err, "Agent A 提交稿件")
	if receiptA1.Status != "pending" {
		t.Fatalf("預期 pending 狀態，實際為 %s", receiptA1.Status)
	}
	t.Logf("【稿件提交】 Agent A 提交 1000 字 → 狀態: %s", receiptA1.Status)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 7：Owner 批准 Agent A 的貢獻
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	draftA := getPendingDraftID(t, conn, cov.CovenantID, agentA.AgentID)
	approveA, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftA,
			"unit_count":       1000,
			"acceptance_ratio": 1.0, // 100% 採納
		})
	mustOK(t, err, "Owner 批准 Agent A")
	t.Logf("【批准稿件】 Agent A 獲得 %d tokens（1000字 × 1.0x）", approveA.TokensAwarded)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 8：Agent B 提交貢獻（業務語義：投稿 800 字，資深作者有 1.5x 加成）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	receiptB1, err := engine.Run(cov.CovenantID, agentB.AgentID, "sess_b",
		&tools.ProposePassage{}, map[string]any{"unit_count": 800})
	mustOK(t, err, "Agent B 提交稿件")
	t.Logf("【稿件提交】 Agent B 提交 800 字 → 狀態: %s", receiptB1.Status)

	// Owner 批准（80% 採納率）
	draftB := getPendingDraftID(t, conn, cov.CovenantID, agentB.AgentID)
	approveB, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftB,
			"unit_count":       800,
			"acceptance_ratio": 0.8, // 80% 採納
		})
	mustOK(t, err, "Owner 批准 Agent B")
	// 預期：floor(800 * 0.8) * 1.5 = floor(640) * 1.5 = 640 * 1.5 = 960
	t.Logf("【批准稿件】 Agent B 獲得 %d tokens（800字 × 80%% × 1.5x 資深加成）", approveB.TokensAwarded)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 邊界案例 1：重複申請同一 platform_id（應被拒絕）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	t.Run("邊界案例_重複加入", func(t *testing.T) {
		// 建立獨立測試用 Covenant（在 OPEN 狀態下測試重複 platform_id）
		dupCov, _, err := covSvc.Create("重複加入測試", "book", "pid_dup_owner")
		mustOK(t, err, "建立測試 Covenant")
		mustOK(t, covSvc.AddTier(dupCov.CovenantID, "tier1", "T1", 1.0, nil), "新增層級")
		dupCov, _ = covSvc.Transition(dupCov.CovenantID, "OPEN")

		// 第一次加入應成功
		_, err = covSvc.Join(dupCov.CovenantID, "pid_same_user", "tier1")
		mustOK(t, err, "第一次加入")

		// 同一個 platform_id 嘗試再次加入（應被拒絕）
		_, err = covSvc.Join(dupCov.CovenantID, "pid_same_user", "tier1")
		if err == nil {
			t.Error("重複加入應被拒絕，但實際成功了")
		} else {
			t.Logf("【邊界驗證】 重複 platform_id 正確被拒絕: %v", err)
		}
	})

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 邊界案例 2：預算超額
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	t.Run("邊界案例_預算超額", func(t *testing.T) {
		// 建立獨立測試用 Covenant，預算設為 25（propose_passage 每次消耗 10）
		budgetCov, budgetOwner, err := covSvc.Create("預算測試專案", "book", "pid_budget_owner")
		mustOK(t, err, "建立預算測試 Covenant")
		mustOK(t, covSvc.AddTier(budgetCov.CovenantID, "tier1", "T1", 1.0, nil), "新增層級")
		budgetCov, _ = covSvc.Transition(budgetCov.CovenantID, "OPEN")
		agent, _ := covSvc.Join(budgetCov.CovenantID, "pid_budget_agent", "tier1")
		budgetCov, _ = covSvc.Transition(budgetCov.CovenantID, "ACTIVE")
		// WI-1: approve agent before it can execute tools.
		_, err = engine.Run(budgetCov.CovenantID, budgetOwner.AgentID, "sess_budget_owner",
			&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
		mustOK(t, err, "approve budget agent")

		// 預算設為 25，每次 propose 消耗 10
		mustOK(t, budget.EnsureCounter(conn, budgetCov.CovenantID, 25.0, "USD"), "設定預算 $25")

		// 第 1、2 次應成功（累計 20）
		for i := 1; i <= 2; i++ {
			_, err := engine.Run(budgetCov.CovenantID, agent.AgentID, "sess_budget",
				&tools.ProposePassage{}, map[string]any{"unit_count": 100})
			mustOK(t, err, "第 %d 次提交", i)
		}

		// 第 3 次應被拒絕（累計 30 > 25）
		_, err = engine.Run(budgetCov.CovenantID, agent.AgentID, "sess_budget",
			&tools.ProposePassage{}, map[string]any{"unit_count": 100})
		if err == nil {
			t.Error("預算超額應被拒絕，但實際成功了")
		} else if !strings.Contains(err.Error(), "budget exhausted") {
			t.Errorf("預期 budget exhausted 錯誤，實際: %v", err)
		} else {
			t.Logf("【邊界驗證】 預算超額正確被拒絕: %v", err)
		}
	})

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 9：鎖定 Covenant（ACTIVE → LOCKED）
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	cov, err = covSvc.Transition(cov.CovenantID, "LOCKED")
	mustOK(t, err, "轉換到 LOCKED")
	t.Log("【狀態轉換】 ACTIVE → LOCKED（停止接受貢獻）")

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 10：產生結算報告
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	settlementReceipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_settle",
		&tools.GenerateSettlement{}, map[string]any{})
	mustOK(t, err, "產生結算報告")

	outputID, _ := settlementReceipt.Extra["output_id"].(string)
	if outputID == "" {
		t.Fatal("結算報告缺少 output_id")
	}
	t.Logf("【結算報告】 Output ID = %s", outputID)

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 10b：AC-8 — confirm_settlement_output → Covenant 狀態到 SETTLED
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	confirmReceipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_settle",
		&tools.ConfirmSettlementOutput{}, map[string]any{
			"settlement_output_id": outputID,
		})
	mustOK(t, err, "confirm_settlement_output")
	if confirmReceipt.Extra["status"] != "SETTLED" {
		t.Errorf("【失敗】 confirm_settlement_output 應回傳 SETTLED，實際=%v", confirmReceipt.Extra["status"])
	} else {
		t.Logf("【AC-8】 confirm_settlement_output 成功 → status=%v", confirmReceipt.Extra["status"])
	}

	// 驗證 Covenant 實際狀態為 SETTLED
	finalState, err := covSvc.State(cov.CovenantID)
	mustOK(t, err, "讀取最終 Covenant 狀態")
	if finalState != "SETTLED" {
		t.Errorf("【失敗】 Covenant 最終狀態應為 SETTLED，實際=%s", finalState)
	} else {
		t.Logf("【AC-8】 Covenant 最終狀態確認: %s", finalState)
	}

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 11：驗證結算比例 — OwnerShare + ContributorPool = 100%
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	var (
		totalTokens        int
		ownerSharePct      float64
		platformSharePct   float64
		contributorPoolPct float64
		distsJSON          string
	)
	err = conn.QueryRow(`
		SELECT total_tokens, owner_share_pct, platform_share_pct, contributor_pool_pct, distributions
		FROM settlement_outputs WHERE output_id = ?`, outputID,
	).Scan(&totalTokens, &ownerSharePct, &platformSharePct, &contributorPoolPct, &distsJSON)
	mustOK(t, err, "讀取結算報告")

	// 解析分配明細
	var dists []struct {
		AgentID       string  `json:"agent_id"`
		InkTokens     int     `json:"ink_tokens"`
		ShareOfPool   float64 `json:"share_of_pool"`
		FinalSharePct float64 `json:"final_share_pct"`
	}
	json.Unmarshal([]byte(distsJSON), &dists)

	t.Logf("【結算明細】")
	t.Logf("  總 Tokens: %d", totalTokens)
	t.Logf("  Owner 佔比: %.2f%%", ownerSharePct)
	t.Logf("  Platform 佔比: %.2f%%", platformSharePct)
	t.Logf("  貢獻者池佔比: %.2f%%", contributorPoolPct)

	// 計算貢獻者總佔比
	var contributorsTotal float64
	for _, d := range dists {
		contributorsTotal += d.FinalSharePct
		t.Logf("  → %s: %d tokens, 池內佔比 %.2f%%, 最終佔比 %.4f%%",
			d.AgentID, d.InkTokens, d.ShareOfPool*100, d.FinalSharePct)
	}

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 核心斷言：Owner + 所有貢獻者 = 100%
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	totalSharePct := ownerSharePct + platformSharePct + contributorsTotal
	tolerance := 0.01 // 允許 0.01% 浮點誤差

	if abs(totalSharePct-100.0) > tolerance {
		t.Errorf("【失敗】 總佔比 %.4f%% != 100%%（誤差超過 %.2f%%）", totalSharePct, tolerance)
	} else {
		t.Logf("【成功】 總佔比驗證通過: %.4f%% ≈ 100%%", totalSharePct)
	}

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 12：驗證 Audit Log 鏈完整性
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	valid, violations := audit.VerifyChain(conn, cov.CovenantID)
	if !valid {
		t.Errorf("【失敗】 Audit Log 鏈損壞: %v", violations)
	} else {
		t.Log("【成功】 Audit Log Hash Chain 驗證通過")
	}

	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	// 步驟 13：驗證 Token 餘額一致性
	// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
	balA, _ := tokens.Balance(conn, cov.CovenantID, agentA.AgentID)
	balB, _ := tokens.Balance(conn, cov.CovenantID, agentB.AgentID)
	ledgerSum, _ := tokens.TotalByAgent(conn, cov.CovenantID)

	var ledgerTotal int
	for _, v := range ledgerSum {
		ledgerTotal += v
	}

	if balA+balB != ledgerTotal {
		t.Errorf("【失敗】 餘額不一致: A(%d) + B(%d) = %d, Ledger Total = %d",
			balA, balB, balA+balB, ledgerTotal)
	} else {
		t.Logf("【成功】 Token 餘額一致: A=%d + B=%d = %d", balA, balB, ledgerTotal)
	}

	if totalTokens != ledgerTotal {
		t.Errorf("【失敗】 結算 TotalTokens(%d) != Ledger Total(%d)", totalTokens, ledgerTotal)
	}
}

// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
// 輔助函式
// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

func mustOK(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(format+": %v", append(args, err)...)
	}
}

func getPendingDraftID(t *testing.T, conn *sql.DB, covenantID, agentID string) string {
	t.Helper()
	var draftID string
	err := conn.QueryRow(`SELECT draft_id FROM pending_tokens WHERE covenant_id=? AND agent_id=? LIMIT 1`,
		covenantID, agentID).Scan(&draftID)
	if err != nil {
		t.Fatalf("找不到 %s 的 pending draft: %v", agentID, err)
	}
	return draftID
}

func intPtr(n int) *int { return &n }

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
