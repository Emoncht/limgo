package garena

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lim-worker-go/pkg/datadome"
)

type Config struct {
	SessionKey         string `json:"session_key"`
	Proxy              string `json:"proxy"`
	BaseURL            string `json:"base_url"`
	AppID              int    `json:"app_id"`
	ChannelID          int    `json:"channel_id"`
	PackedRoleID       int    `json:"packed_role_id"`
	ServiceVersion     string `json:"service_version"`
	MaxShellCost       int    `json:"max_shell_cost"`
	MinDiscountPercent int    `json:"min_discount_percent"`
}

type MerchantInfo struct {
	UID          int64  `json:"uid"`
	ShellBalance int    `json:"shell_balance"`
	SessionKey   string `json:"session_key"`
	Error        string `json:"error,omitempty"`
}

type PlayerLoginResult struct {
	Nickname string `json:"nickname"`
	Region   string `json:"region"`
	Error    string `json:"error,omitempty"`
}

type EventPricingData struct {
	EventInfo struct {
		ID           int                    `json:"id"`
		Status       int                    `json:"status"`
		EligibleItem int                    `json:"eligible_item"`
		CurrencyDict map[string]interface{} `json:"currency_dict"`
	} `json:"event_info"`
	Channels []struct {
		Channel int `json:"channel"`
		Items   []struct {
			ItemID         int `json:"item_id"`
			CurrencyAmount int `json:"currency_amount"`
		} `json:"items"`
	} `json:"channels"`
}

type VerificationResult struct {
	Eligible        bool   `json:"eligible"`
	ErrorCode       string `json:"error_code,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EligibleItemID  int    `json:"eligible_item_id"`
	EventID         int    `json:"event_id"`
	ShellCost       int    `json:"shell_cost"`
	OriginalPrice   int    `json:"original_price"`
	DiscountPercent int    `json:"discount_percent"`
}

func LoginGarena(cfg Config, client *http.Client) (MerchantInfo, error) {
	req, err := http.NewRequest("GET", cfg.BaseURL+"/api/auth/check_session", nil)
	if err != nil {
		return MerchantInfo{}, err
	}
	req.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s;", cfg.SessionKey))

	resp, err := client.Do(req)
	if err != nil {
		return MerchantInfo{}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	fmt.Printf("[GoWorker] 📡 GET /api/auth/check_session [%d]: %s\n", resp.StatusCode, string(body))

	var sess struct {
		SessionKey string `json:"session_key"`
		Login      bool   `json:"login"`
		Error      string `json:"error"`
	}
	json.Unmarshal(body, &sess)

	if sess.SessionKey == "" || !sess.Login {
		return MerchantInfo{Error: "error_require_login"}, nil
	}

	// SSO Step
	ssoPayload, _ := json.Marshal(map[string]string{"session_key": sess.SessionKey})
	ssoReq, err := http.NewRequest("POST", cfg.BaseURL+"/api/auth/sso", bytes.NewBuffer(ssoPayload))
	if err != nil {
		return MerchantInfo{}, err
	}
	ssoReq.Header.Set("Content-Type", "application/json")
	ssoReq.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s;", cfg.SessionKey))

	ssoResp, err := client.Do(ssoReq)
	if err != nil {
		return MerchantInfo{}, err
	}
	ssoBody, _ := io.ReadAll(ssoResp.Body)
	ssoResp.Body.Close()

	fmt.Printf("[GoWorker] 📡 POST /api/auth/sso [%d]: %s\n", ssoResp.StatusCode, string(ssoBody))

	var ssoRes struct {
		UID          int64  `json:"uid"`
		ShellBalance int    `json:"shell_balance"`
		Error        string `json:"error"`
	}
	json.Unmarshal(ssoBody, &ssoRes)

	return MerchantInfo{
		UID:          ssoRes.UID,
		ShellBalance: ssoRes.ShellBalance,
		SessionKey:   cfg.SessionKey,
		Error:        ssoRes.Error,
	}, nil
}

func LoginPlayerWithRetry(gameID string, cfg Config, defaultClient *http.Client) (PlayerLoginResult, error) {
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		var proxyClient *http.Client
		if cfg.Proxy != "" {
			proxyURLStr := cfg.Proxy
			if strings.Contains(proxyURLStr, "gw.dataimpulse.com") {
				sessID := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
				u, err := url.Parse(proxyURLStr)
				if err == nil && u.User != nil {
					user := u.User.Username()
					pass, _ := u.User.Password()
					// Strip existing session info if present, then append fresh session ID
					if idx := strings.Index(user, ";sessttl"); idx != -1 {
						user = user[:idx]
					}
					newUser := fmt.Sprintf("%s;sessttl.5;sessid.%s", user, sessID)
					u.User = url.UserPassword(newUser, pass)
					proxyURLStr = u.String()
				}
			}
			proxyURL, err := url.Parse(proxyURLStr)
			if err == nil {
				transport := &http.Transport{
					Proxy: http.ProxyURL(proxyURL),
				}
				proxyClient = &http.Client{
					Transport: transport,
					Timeout:   25 * time.Second,
				}
			}
		}
		if proxyClient == nil {
			proxyClient = defaultClient
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"app_id":   cfg.AppID,
			"login_id": gameID,
		})

		ddResult, _ := datadome.GenerateCookie(cfg.BaseURL+"/api/auth/player_id_login", proxyClient)

		req, err := http.NewRequest("POST", cfg.BaseURL+"/api/auth/player_id_login", bytes.NewBuffer(payload))
		if err != nil {
			return PlayerLoginResult{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
		req.Header.Set("Origin", cfg.BaseURL)
		req.Header.Set("Referer", cfg.BaseURL+"/")
		req.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s; datadome=%s;", cfg.SessionKey, ddResult.ClientID))

		resp, err := proxyClient.Do(req)
		if err != nil {
			fmt.Printf("[GoWorker] ❌ POST /api/auth/player_id_login Attempt %d/%d HTTP Error: %v\n", attempt, maxRetries, err)
			if attempt < maxRetries {
				time.Sleep(2 * time.Second)
				continue
			}
			return PlayerLoginResult{Error: err.Error()}, nil
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		fmt.Printf("[GoWorker] 📡 POST /api/auth/player_id_login [%d] Attempt %d/%d: %s\n", resp.StatusCode, attempt, maxRetries, string(body))

		var res struct {
			Nickname string `json:"nickname"`
			Region   string `json:"region"`
			Error    string `json:"error"`
		}
		json.Unmarshal(body, &res)

		if res.Error == "invalid_id" {
			return PlayerLoginResult{Error: "invalid_id"}, nil
		}
		if resp.StatusCode == 200 && res.Error == "" && res.Nickname != "" {
			return PlayerLoginResult{Nickname: res.Nickname, Region: res.Region}, nil
		}

		if attempt < maxRetries {
			time.Sleep(2 * time.Second)
		} else {
			errReason := res.Error
			if errReason == "" {
				errReason = fmt.Sprintf("DataDome captcha block (HTTP %d)", resp.StatusCode)
			}
			return PlayerLoginResult{Error: errReason}, nil
		}
	}

	return PlayerLoginResult{Error: "Login failed after max retries"}, nil
}

func GetEventPricing(cfg Config, client *http.Client) (EventPricingData, error) {
	urlStr := fmt.Sprintf("%s/api/shop/apps/event_pricing?app_id=%d&packed_role_id=%d&region=BD&language=en&event_type=1&event_region=BD&_t=%d",
		cfg.BaseURL, cfg.AppID, cfg.PackedRoleID, time.Now().UnixMilli())

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return EventPricingData{}, err
	}
	req.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s;", cfg.SessionKey))

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[GoWorker] ❌ GET /api/shop/apps/event_pricing HTTP Error: %v\n", err)
		return EventPricingData{}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	fmt.Printf("[GoWorker] 📡 GET /api/shop/apps/event_pricing [%d]: %s\n", resp.StatusCode, string(body))

	var data EventPricingData
	err = json.Unmarshal(body, &data)
	return data, err
}

func VerifyShellCost(pricing EventPricingData, cfg Config) VerificationResult {
	eventInfo := pricing.EventInfo
	fmt.Printf("[GoWorker] 🔍 VerifyShellCost Debug — event_id: %d, status: %d, eligible_item: %d, currency_dict: %v\n",
		eventInfo.ID, eventInfo.Status, eventInfo.EligibleItem, eventInfo.CurrencyDict)

	if eventInfo.ID == 0 {
		return VerificationResult{Eligible: false, Reason: "No event_info in pricing response"}
	}
	if eventInfo.Status != 0 {
		return VerificationResult{
			Eligible:  false,
			ErrorCode: "error_lim_not_eligible",
			Reason:    fmt.Sprintf("Event status is %d (not eligible/available)", eventInfo.Status),
		}
	}

	eligibleItemID := eventInfo.EligibleItem
	eventID := eventInfo.ID

	var originalPrice float64
	if gsVal, ok := eventInfo.CurrencyDict["GS"]; ok {
		switch v := gsVal.(type) {
		case float64:
			originalPrice = v
		case int:
			originalPrice = float64(v)
		}
	}

	if eligibleItemID == 0 || originalPrice == 0 {
		return VerificationResult{Eligible: false, Reason: "Missing eligible_item or original price in event_info"}
	}

	var shellCost int = -1
	for _, ch := range pricing.Channels {
		if ch.Channel == cfg.ChannelID {
			for _, item := range ch.Items {
				if item.ItemID == eligibleItemID {
					shellCost = item.CurrencyAmount
					break
				}
			}
		}
	}

	if shellCost == -1 {
		return VerificationResult{Eligible: false, Reason: "Item not found in channel items"}
	}

	intOriginalPrice := int(originalPrice)
	discountPercent := ((intOriginalPrice - shellCost) * 100) / intOriginalPrice

	res := VerificationResult{
		EligibleItemID:  eligibleItemID,
		EventID:         eventID,
		ShellCost:       shellCost,
		OriginalPrice:   intOriginalPrice,
		DiscountPercent: discountPercent,
	}

	if shellCost <= cfg.MaxShellCost || discountPercent >= cfg.MinDiscountPercent {
		res.Eligible = true
		return res
	}

	res.Eligible = false
	res.ErrorCode = "error_lim_too_expensive"
	res.Reason = fmt.Sprintf("Shell cost %d exceeds max %d and discount %d%% is below min %d%%",
		shellCost, cfg.MaxShellCost, discountPercent, cfg.MinDiscountPercent)
	return res
}

func GetCSRFToken(cfg Config, client *http.Client) (string, error) {
	req, err := http.NewRequest("POST", cfg.BaseURL+"/api/preflight", bytes.NewBufferString("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", fmt.Sprintf("source=pc; session_key=%s;", cfg.SessionKey))

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__csrf__" {
			return cookie.Value, nil
		}
	}

	// Fallback to set-cookie header parsing
	for _, header := range resp.Header["Set-Cookie"] {
		if strings.Contains(header, "__csrf__=") {
			parts := strings.Split(header, ";")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "__csrf__=") {
					return strings.TrimPrefix(p, "__csrf__="), nil
				}
			}
		}
	}

	return strconv.FormatInt(time.Now().UnixNano(), 10), nil
}
