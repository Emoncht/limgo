package datadome

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type DataDomeResult struct {
	Cookie   string `json:"cookie"`
	ClientID string `json:"client_id"`
}

// GenerateCookie calls api-js.datadome.co to get a fresh DataDome cookie
func GenerateCookie(targetURL string, client *http.Client) (DataDomeResult, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	u, err := url.Parse(targetURL)
	reqPath := "/"
	if err == nil && u.Path != "" {
		reqPath = u.Path
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	randomIP := fmt.Sprintf("%d.%d.%d.%d", r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256))

	jsDataObj := map[string]interface{}{
		"plg": 0, "plgod": false, "plgne": "NA", "plgre": "NA", "plgof": "NA",
		"plggt": "NA", "pltod": false,
		"br_h": 937, "br_w": 1920, "br_oh": 937, "br_ow": 1920,
		"jsf": false, "cvs": nil, "phe": false, "nm": false,
		"sln": nil, "lo": true, "lb": true,
		"hc": 8, "rs_h": 1080, "rs_w": 1920, "rs_cd": 24,
		"ua": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
		"lg": "en-GB", "pr": 1, "ars_h": 1040, "ars_w": 1920,
		"tz": -480, "tzp": "Asia/Kuala_Lumpur",
		"str_ss": true, "str_ls": true, "str_idb": true, "str_odb": true,
		"so": "landscape-primary", "wdw": true,
		"vnd": "Google Inc.", "mmt": "empty", "plu": "empty",
		"hdn": false, "awe": false, "geb": false, "dat": false,
		"eva": 33, "med": "defined",
		"dcok": ".bdgamesbazar.com",
	}

	jsDataBytes, _ := json.Marshal(jsDataObj)

	eventCountersObj := map[string]interface{}{
		"mousemove": r.Intn(50) + 20,
		"click":     r.Intn(5) + 1,
		"scroll":    r.Intn(3),
		"keydown":   r.Intn(10),
		"keyup":     r.Intn(10),
	}
	eventCountersBytes, _ := json.Marshal(eventCountersObj)

	form := url.Values{}
	form.Set("jsData", string(jsDataBytes))
	form.Set("events", "[]")
	form.Set("eventCounters", string(eventCountersBytes))
	form.Set("jsType", "ch")
	form.Set("cid", "7wjS722f1LrDsyaQa9pBI2nWnmLK8ksSQvrb.ojDP83oOy~..jUPYAdXD7I823mKpXqXARYE8tBZzFr98tq3KQlH9JgTLC.XkWk~zt5U1X")
	form.Set("ddk", "AE3F04AD3F0D3A462481A337485081")
	form.Set("Referer", targetURL)
	form.Set("request", reqPath)
	form.Set("responsePage", "origin")
	form.Set("ddv", "5.1.11")

	req, err := http.NewRequest("POST", "https://api-js.datadome.co/js/", strings.NewReader(form.Encode()))
	if err != nil {
		return DataDomeResult{}, err
	}

	req.Header.Set("x-forwarded-for", randomIP)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Host", "api-js.datadome.co")
	req.Header.Set("Origin", targetURL)
	req.Header.Set("Referer", targetURL)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return DataDomeResult{}, err
	}
	defer resp.Body.Close()

	var ddResp struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ddResp); err != nil {
		return DataDomeResult{}, err
	}

	clientID := ""
	if strings.Contains(ddResp.Cookie, "datadome=") {
		parts := strings.Split(ddResp.Cookie, ";")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "datadome=") {
				clientID = strings.TrimPrefix(p, "datadome=")
				break
			}
		}
	}

	return DataDomeResult{
		Cookie:   ddResp.Cookie,
		ClientID: clientID,
	}, nil
}
