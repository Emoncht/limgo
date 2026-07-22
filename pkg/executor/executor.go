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
	SessionKey string              `json:"session_key"`
	FAToken    string              `json:"fa_token"`
	Proxy      string              `json:"proxy"`
	Players    []PlayerOrder       `json:"players"`
	Config     garena.Config       `json:"config"`
}

type ItemResult struct {
	RefID    string      `json:"refid"`
	Status   string      `json:"status"`
	GameID   string      `json:"game_id"`
	Nickname string      `json:"nickname,omitempty"`
	Region   string      `json:"region,omitempty"`
	Response interface{} `json:"response"`
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

	// Configure HTTP client with a warm connection pool (high MaxConns per host)
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

	// 1. Authenticate Merchant
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

	// Group players by GameID
	playerGroups := make(map[string][]PlayerOrder)
	for _, p := range payload.Players {
		playerGroups[p.GameID] = append(playerGroups[p.GameID], p)
	}

	var finalResults []ItemResult
	var resultsMutex sync.Mutex

	for gameID, group := range playerGroups {
		fmt.Printf("[GoWorker] 🔒 Processing group for Player: %s (%d orders)\n", gameID, len(group))

		// 2. Player Login
		playerInfo, err := garena.LoginPlayerWithRetry(gameID, cfg, fastClient)
		if err != nil || playerInfo.Error != "" {
			errStr := playerInfo.Error
			if errStr == "" && err != nil {
				errStr = err.Error()
			}
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

		// 3. Event Pricing & Shell Verification
		pricing, _ := garena.GetEventPricing(cfg, fastClient)
		verification := garena.VerifyShellCost(pricing, cfg)

		if !verification.Eligible && verification.ErrorCode != "" {
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
		}

		// 4. Generate OTP
		otpCode, err := totp.GenerateOTP(payload.FAToken)
		if err != nil {
			fmt.Printf("[GoWorker] ⚠️ TOTP error: %v, falling back to default\n", err)
			otpCode = "000000"
		}

		// 5. Fetch CSRF token & DataDome cookie concurrently
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

		// 6. Pre-heat TCP/TLS connections to bdgamesbazar.com
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

		// 7. Pre-build request payloads & raw JSON bytes
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

		// 8. SYNCHRONIZED GOROUTINE BURST ENGINE
		// Using a closed channel barrier for zero-jitter microsecond launch
		startChan := make(chan struct{})
		var burstWG sync.WaitGroup
		groupResults := make([]ItemResult, len(group))

		payInitURL := cfg.BaseURL + "/api/shop/pay/init?region=BD&language=en"

		for i, item := range reqItems {
			burstWG.Add(1)
			go func(idx int, ri ReqItem) {
				defer burstWG.Done()

				// Synchronized Barrier Wait
				<-startChan

				// Instant launch!
				req, err := http.NewRequest("POST", payInitURL, bytes.NewReader(ri.PayloadBytes))
				if err != nil {
					groupResults[idx] = ItemResult{
						RefID:    ri.Player.RefID,
						Status:   "Failed",
						GameID:   ri.Player.GameID,
						Nickname: playerInfo.Nickname,
						Region:   playerInfo.Region,
						Response: map[string]string{"error": err.Error()},
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
				if err != nil {
					groupResults[idx] = ItemResult{
						RefID:    ri.Player.RefID,
						Status:   "Failed",
						GameID:   ri.Player.GameID,
						Nickname: playerInfo.Nickname,
						Region:   playerInfo.Region,
						Response: map[string]string{"error": err.Error()},
					}
					return
				}

				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				var resObj map[string]interface{}
				json.Unmarshal(body, &resObj)

				resultStatus := "Failed"
				if resObj != nil {
					if r, ok := resObj["result"].(string); ok && r == "success" {
						resultStatus = "Success"
					} else if r == "error_2sa_otp" {
						resultStatus = "Processing"
					}
				}

				groupResults[idx] = ItemResult{
					RefID:    ri.Player.RefID,
					Status:   resultStatus,
					GameID:   ri.Player.GameID,
					Nickname: playerInfo.Nickname,
					Region:   playerInfo.Region,
					Response: resObj,
				}
			}(i, item)
		}

		// ⚡⚡ UNLEASH ALL GOROUTINES AT THE EXACT SAME MICROSECOND ⚡⚡
		fireStart := time.Now()
		close(startChan)

		// Wait for all burst requests to finish
		burstWG.Wait()
		fireDuration := time.Since(fireStart).Milliseconds()
		fmt.Printf("⚡ [GoWorker] All %d pay/init requests completed in %dms!\n", len(group), fireDuration)

		resultsMutex.Lock()
		finalResults = append(finalResults, groupResults...)
		resultsMutex.Unlock()
	}

	// Post-execution shell balance check
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
