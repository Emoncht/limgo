package executor

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"lim-worker-go/pkg/datadome"
	"lim-worker-go/pkg/garena"
	"lim-worker-go/pkg/totp"
)

type PlayerOrder struct {
	RefID  string `json:"refid"`
	GameID string `json:"game_id"`
}

type Payload struct {
	SessionKey string        `json:"session_key"`
	FAToken    string        `json:"fa_token"`
	Proxy      string        `json:"proxy"`
	Players    []PlayerOrder `json:"players"`
	Config     garena.Config `json:"config"`
}

type ItemResult struct {
	RefID         string      `json:"refid"`
	Status        string      `json:"status"`
	GameID        string      `json:"game_id"`
	Nickname      string      `json:"nickname,omitempty"`
	Region        string      `json:"region,omitempty"`
	DurationMs    int64       `json:"duration_ms"`
	StartOffsetMs int64       `json:"start_offset_ms"`
	Response      interface{} `json:"response"`
}

type ExecuteResult struct {
	Success            bool         `json:"success"`
	Error              string       `json:"error,omitempty"`
	ShellBalanceBefore int          `json:"shell_balance_before"`
	ShellBalanceAfter  int          `json:"shell_balance_after"`
	FireDurationMs     int64        `json:"fire_duration_ms"`
	Results            []ItemResult `json:"results"`
}

func randomSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Execute(payload Payload) ExecuteResult {
	startTime := time.Now()
	cfg := payload.Config
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://bdgamesbazar.com"
	}
	if cfg.AppID == 0 {
		cfg.AppID = 100067
	}
	if cfg.ChannelID == 0 {
		cfg.ChannelID = 221070
	}
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "mshop_frontend_20260713"
	}
	if cfg.MaxShellCost == 0 {
		cfg.MaxShellCost = 90
	}
	if cfg.MinDiscountPercent == 0 {
		cfg.MinDiscountPercent = 63
	}
	cfg.SessionKey = payload.SessionKey
	cfg.Proxy = payload.Proxy

	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		MaxConnsPerHost:     200,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}

	fastClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	fmt.Printf("[GoWorker] 🚀 Processing %d player(s)\n", len(payload.Players))

	merchant, err := garena.LoginGarena(cfg, fastClient)
	if err != nil {
		return ExecuteResult{
			Success: false,
			Error:   fmt.Sprintf("Merchant login request error: %v", err),
		}
	}
	if merchant.SessionKey != "" {
		cfg.SessionKey = merchant.SessionKey
	}
	shellBalanceBefore := merchant.ShellBalance
	fmt.Printf("[GoWorker] 🔐 Merchant authenticated — UID: %d, Shell Balance: %d\n", merchant.UID, shellBalanceBefore)

	playerGroups := make(map[string][]PlayerOrder)
	for _, p := range payload.Players {
		playerGroups[p.GameID] = append(playerGroups[p.GameID], p)
	}

	var finalResults []ItemResult
	var resultsMutex sync.Mutex

	for gameID, group := range playerGroups {
		fmt.Printf("[GoWorker] 🔒 Processing group for Player: %s (%d order(s))\n", gameID, len(group))

		playerInfo, err := garena.LoginPlayerWithRetry(gameID, cfg, fastClient)
		if err != nil || playerInfo.Error != "" {
			errStr := playerInfo.Error
			if errStr == "" && err != nil {
				errStr = err.Error()
			}
			fmt.Printf("[GoWorker] ❌ Player login failed for %s: %s\n", gameID, errStr)
			for _, p := range group {
				finalResults = append(finalResults, ItemResult{
					RefID:    p.RefID,
					Status:   "Failed",
					GameID:   p.GameID,
					Response: map[string]string{"error": errStr},
				})
			}
			continue
		}
		fmt.Printf("[GoWorker] 🎮 Player logged in: Nickname=%s, Region=%s\n", playerInfo.Nickname, playerInfo.Region)

		pricing, _ := garena.GetEventPricing(cfg, fastClient)
		verification := garena.VerifyShellCost(pricing, cfg)

		if !verification.Eligible && verification.ErrorCode != "" {
			fmt.Printf("[GoWorker] ❌ Shell verification failed: %s\n", verification.Reason)
			for _, p := range group {
				finalResults = append(finalResults, ItemResult{
					RefID:    p.RefID,
					Status:   "Failed",
					GameID:   p.GameID,
					Nickname: playerInfo.Nickname,
					Region:   playerInfo.Region,
					Response: map[string]interface{}{
						"error":            verification.ErrorCode,
						"message":          verification.Reason,
						"shell_cost":       verification.ShellCost,
						"original_price":   verification.OriginalPrice,
						"discount_percent": verification.DiscountPercent,
					},
				})
			}
			continue
		}

		useItemID := verification.EligibleItemID
		useEventID := verification.EventID
		if !verification.Eligible {
			useItemID = 2883
			useEventID = pricing.EventInfo.ID
			fmt.Printf("[GoWorker] ⚠️ Shell verification ineligible (%s), attempting anyway...\n", verification.Reason)
		} else {
			fmt.Printf("[GoWorker] ✅ Shell verification PASSED: item_id=%d, shell_cost=%d (%d%% discount)\n",
				useItemID, verification.ShellCost, verification.DiscountPercent)
		}

		otpCode, err := totp.GenerateOTP(payload.FAToken)
		if err != nil {
			fmt.Printf("[GoWorker] ⚠️ TOTP error: %v, falling back to default\n", err)
			otpCode = "000000"
		}
		fmt.Printf("[GoWorker] 🔢 OTP generated: %s\n", otpCode)

		var csrfToken string
		var ddResult datadome.DataDomeResult
		var preWG sync.WaitGroup
		preWG.Add(2)

		go func() {
			defer preWG.Done()
			csrfToken, _ = garena.GetCSRFToken(cfg, fastClient)
		}()

		go func() {
			defer preWG.Done()
			ddResult, _ = datadome.GenerateCookie(cfg.BaseURL+"/api/auth/player_id_login", fastClient)
		}()

		preWG.Wait()
		fmt.Printf("[GoWorker] 🔑 CSRF and DataDome pre-fetched (DataDome CID: %s...)\n", ddResult.ClientID[:min(20, len(ddResult.ClientID))])

		fmt.Printf("[GoWorker] 🔥 Pre-heating %d TCP/TLS connections...\n", len(group))
		var heatWG sync.WaitGroup
		for i := 0; i < len(group); i++ {
			heatWG.Add(1)
			go func() {
				defer heatWG.Done()
				req, _ := http.NewRequest("GET", cfg.BaseURL+"/api/preflight", nil)
				req.Header.Set("Cookie", fmt.Sprintf("session_key=%s", cfg.SessionKey))
				resp, err := fastClient.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}
		heatWG.Wait()
		fmt.Println("[GoWorker] 🔥 Connections pre-warmed in pool.")

		sharedSessionID := randomSessionID()
		type ReqItem struct {
			Player       PlayerOrder
			PayloadBytes []byte
		}
		reqItems := make([]ReqItem, len(group))

		for i, p := range group {
			payloadData := map[string]interface{}{
				"app_id":         cfg.AppID,
				"packed_role_id": cfg.PackedRoleID,
				"channel_id":     cfg.ChannelID,
				"service":        "pc",
				"item_id":        useItemID,
				"channel_data": map[string]interface{}{
					"otp_code":   otpCode,
					"garena_uid": merchant.UID,
				},
				"event_id": useEventID,
				"revamp_experiment": map[string]interface{}{
					"session_id":      sharedSessionID,
					"group":           "treatment2",
					"service_version": cfg.ServiceVersion,
					"source":          "pc",
					"domain":          "bdgamesbazar.com",
				},
			}
			pb, _ := json.Marshal(payloadData)
			reqItems[i] = ReqItem{Player: p, PayloadBytes: pb}
		}

		startChan := make(chan struct{})
		var burstWG sync.WaitGroup
		groupResults := make([]ItemResult, len(group))

		payInitURL := cfg.BaseURL + "/api/shop/pay/init?region=BD&language=en"
		var fireStart time.Time

		for i, item := range reqItems {
			burstWG.Add(1)
			go func(idx int, ri ReqItem) {
				defer burstWG.Done()

				<-startChan
				reqLaunchTime := time.Now()
				startOffsetMs := reqLaunchTime.Sub(fireStart).Milliseconds()

				req, err := http.NewRequest("POST", payInitURL, bytes.NewReader(ri.PayloadBytes))
				if err != nil {
					durationMs := time.Since(reqLaunchTime).Milliseconds()
					fmt.Printf("[GoWorker] 🔫 Fire #%d [%s]: offset=%dms, duration=%dms, error=%v\n",
						idx+1, ri.Player.RefID, startOffsetMs, durationMs, err)
					groupResults[idx] = ItemResult{
						RefID:         ri.Player.RefID,
						Status:        "Failed",
						GameID:        ri.Player.GameID,
						Nickname:      playerInfo.Nickname,
						Region:        playerInfo.Region,
						DurationMs:    durationMs,
						StartOffsetMs: startOffsetMs,
						Response:      map[string]string{"error": err.Error()},
					}
					return
				}

				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json")
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
				req.Header.Set("Origin", cfg.BaseURL)
				req.Header.Set("Referer", cfg.BaseURL+"/")
				req.Header.Set("X-Csrf-Token", csrfToken)
				req.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s; datadome=%s; __csrf__=%s",
					cfg.SessionKey, ddResult.ClientID, csrfToken))

				resp, err := fastClient.Do(req)
				durationMs := time.Since(reqLaunchTime).Milliseconds()

				if err != nil {
					fmt.Printf("[GoWorker] 🔫 Fire #%d [%s]: offset=%dms, duration=%dms, error=%v\n",
						idx+1, ri.Player.RefID, startOffsetMs, durationMs, err)
					groupResults[idx] = ItemResult{
						RefID:         ri.Player.RefID,
						Status:        "Failed",
						GameID:        ri.Player.GameID,
						Nickname:      playerInfo.Nickname,
						Region:        playerInfo.Region,
						DurationMs:    durationMs,
						StartOffsetMs: startOffsetMs,
						Response:      map[string]string{"error": err.Error()},
					}
					return
				}

				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				var resObj map[string]interface{}
				json.Unmarshal(body, &resObj)

				resultStatus := "Failed"
				displayID := "0"
				resCode := "unknown"

				if resObj != nil {
					if r, ok := resObj["result"].(string); ok {
						resCode = r
						if r == "success" {
							resultStatus = "Success"
						} else if r == "error_2sa_otp" {
							resultStatus = "Processing"
						}
					}
					if d, ok := resObj["display_id"].(string); ok {
						displayID = d
					}
				}

				fmt.Printf("[GoWorker] 🔫 Fire #%d [%s]: offset=%dms, duration=%dms, result=%s, display_id=%s\n",
					idx+1, ri.Player.RefID, startOffsetMs, durationMs, resCode, displayID)

				groupResults[idx] = ItemResult{
					RefID:         ri.Player.RefID,
					Status:        resultStatus,
					GameID:        ri.Player.GameID,
					Nickname:      playerInfo.Nickname,
					Region:        playerInfo.Region,
					DurationMs:    durationMs,
					StartOffsetMs: startOffsetMs,
					Response:      resObj,
				}
			}(i, item)
		}

		fireStart = time.Now()
		close(startChan)

		burstWG.Wait()
		fireDuration := time.Since(fireStart).Milliseconds()

		var succCount, ineligCount, failCount int
		for _, res := range groupResults {
			if res.Status == "Success" {
				succCount++
			} else if respMap, ok := res.Response.(map[string]interface{}); ok && respMap["result"] == "error_init_event_not_eligible" {
				ineligCount++
			} else {
				failCount++
			}
		}

		fmt.Printf("⚡ [GoWorker] Burst completed in %dms! Total: %d | Success: %d | Ineligible: %d | Other Failed: %d\n",
			fireDuration, len(group), succCount, ineligCount, failCount)

		resultsMutex.Lock()
		finalResults = append(finalResults, groupResults...)
		resultsMutex.Unlock()
	}

	postMerchant, _ := garena.LoginGarena(cfg, fastClient)
	shellBalanceAfter := postMerchant.ShellBalance
	if shellBalanceAfter == 0 && postMerchant.Error != "" {
		shellBalanceAfter = shellBalanceBefore
	}

	return ExecuteResult{
		Success:            true,
		ShellBalanceBefore: shellBalanceBefore,
		ShellBalanceAfter:  shellBalanceAfter,
		FireDurationMs:     time.Since(startTime).Milliseconds(),
		Results:            finalResults,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
