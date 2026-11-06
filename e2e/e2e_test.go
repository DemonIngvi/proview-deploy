//go:build e2e

// Package e2e drives the whole ProView stack through its public interfaces,
// exactly as a browser and an admin would:
//
//	web (nginx) -> banner-service -> event-collector -> Kafka -> event-ingester
//	-> ClickHouse (raw + aggregates) -> analytics API
//
// Run against docker compose (scripts/e2e.sh) or any deployed environment:
//
//	E2E_WEB_URL=http://localhost:8080 E2E_ANALYTICS_URL=http://localhost:8090 \
//	E2E_BANNER_ADMIN_URL=http://localhost:8081 go test -tags e2e -v ./...
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

var client = &http.Client{Timeout: 10 * time.Second}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	webURL       = env("E2E_WEB_URL", "http://localhost:8080")
	analyticsURL = env("E2E_ANALYTICS_URL", "http://localhost:8090")
	adminURL     = env("E2E_BANNER_ADMIN_URL", "http://localhost:8081")
	adminToken   = env("E2E_BANNER_ADMIN_TOKEN", "dev-banner-admin-token")
	analyticsPwd = env("E2E_ADMIN_PASSWORD", "admin")
)

type bannerResp struct {
	ImpressionID string `json:"impression_id"`
	Token        string `json:"token"`
	Placement    string `json:"placement"`
	Banner       struct {
		ID       int64  `json:"id"`
		ImageURL string `json:"image_url"`
	} `json:"banner"`
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", browserUA)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func getBanner(t *testing.T, placement, country, ua string) (int, bannerResp) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, webURL+"/v1/banner?placement="+placement, nil)
	req.Header.Set("X-Country", country)
	req.Header.Set("User-Agent", ua)
	resp, body := do(t, req)
	var b bannerResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &b); err != nil {
			t.Fatalf("decode banner: %v: %s", err, body)
		}
	}
	return resp.StatusCode, b
}

type ev struct {
	Type       string `json:"type"`
	TS         int64  `json:"ts"`
	Seq        int    `json:"seq"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

func sendEvents(t *testing.T, token, visitor string, events []ev) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"visitor_id": visitor, "page_url": webURL + "/",
		"items": []map[string]any{{"token": token, "events": events}},
	})
	req, _ := http.NewRequest(http.MethodPost, webURL+"/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8") // what navigator.sendBeacon sends
	req.Header.Set("X-Country", "RU")
	resp, rb := do(t, req)
	return resp.StatusCode, string(rb)
}

type metrics struct {
	Impressions uint64  `json:"impressions"`
	Viewable    uint64  `json:"viewable"`
	Clicks      uint64  `json:"clicks"`
	Hovers      uint64  `json:"hovers"`
	HoverMS     uint64  `json:"hover_ms"`
	ActiveMS    uint64  `json:"active_ms"`
	Visitors    uint64  `json:"visitors"`
	CTR         float64 `json:"ctr"`
}

func analytics(t *testing.T, path string, q url.Values, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, analyticsURL+path+"?"+q.Encode(), nil)
	req.SetBasicAuth("admin", analyticsPwd)
	resp, body := do(t, req)
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

func summary(t *testing.T, bannerID int64, from time.Time) metrics {
	t.Helper()
	q := url.Values{"from": {from.UTC().Format(time.RFC3339)}, "to": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		"banner": {fmt.Sprint(bannerID)}, "country": {"RU"}}
	var s struct {
		Current metrics `json:"current"`
	}
	if code := analytics(t, "/api/v1/summary", q, &s); code != http.StatusOK {
		t.Fatalf("summary: HTTP %d", code)
	}
	return s.Current
}

func waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		code, _ := getBanner(t, "home_top", "RU", browserUA)
		req, _ := http.NewRequest(http.MethodGet, analyticsURL+"/api/v1/summary", nil)
		req.SetBasicAuth("admin", analyticsPwd)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if code == http.StatusOK && resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("stack not ready within 3 minutes")
}

// TestEventPipeline is the core scenario: every event type sent by a
// "browser" shows up, correctly aggregated, in the analytics API.
func TestEventPipeline(t *testing.T) {
	waitReady(t)
	start := time.Now().Add(-time.Minute)

	_, b := getBanner(t, "home_top", "RU", browserUA)
	if b.Token == "" || b.Banner.ID == 0 || b.Placement != "home_top" {
		t.Fatalf("unexpected banner response %+v", b)
	}
	before := summary(t, b.Banner.ID, start)

	now := time.Now().UnixMilli()
	visitor := fmt.Sprintf("e2e-%d", now)
	code, body := sendEvents(t, b.Token, visitor, []ev{
		{Type: "impression", TS: now, Seq: 0},
		{Type: "viewable", TS: now + 1000, Seq: 1},
		{Type: "hover", TS: now + 2000, Seq: 2, DurationMS: 640},
		{Type: "click", TS: now + 2100, Seq: 3},
		{Type: "active_time", TS: now + 5000, Seq: 4, DurationMS: 5000},
	})
	if code != http.StatusAccepted || !strings.Contains(body, `"accepted":5`) {
		t.Fatalf("collector: HTTP %d %s", code, body)
	}

	deadline := time.Now().Add(90 * time.Second)
	var after metrics
	for time.Now().Before(deadline) {
		after = summary(t, b.Banner.ID, start)
		if after.Clicks >= before.Clicks+1 && after.ActiveMS >= before.ActiveMS+5000 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	diff := func(a, b uint64) uint64 { return a - b }
	if diff(after.Impressions, before.Impressions) < 1 || diff(after.Viewable, before.Viewable) < 1 ||
		diff(after.Clicks, before.Clicks) < 1 || diff(after.Hovers, before.Hovers) < 1 ||
		diff(after.HoverMS, before.HoverMS) < 640 || diff(after.ActiveMS, before.ActiveMS) < 5000 {
		t.Fatalf("events not aggregated in time:\nbefore %+v\nafter  %+v", before, after)
	}
	t.Logf("pipeline OK: before=%+v after=%+v", before, after)

	// Dashboard endpoints serve the same data.
	q := url.Values{"from": {start.UTC().Format(time.RFC3339)}, "group_by": {"banner"}, "granularity": {"minute"}}
	var ts struct {
		Buckets []int64 `json:"buckets"`
		Series  []struct {
			Key   string  `json:"key"`
			Label string  `json:"label"`
			Total metrics `json:"total"`
		} `json:"series"`
	}
	if code := analytics(t, "/api/v1/timeseries", q, &ts); code != http.StatusOK || len(ts.Buckets) == 0 || len(ts.Series) == 0 {
		t.Fatalf("timeseries: HTTP %d %+v", code, ts)
	}
	found := false
	for _, s := range ts.Series {
		if s.Key == fmt.Sprint(b.Banner.ID) {
			found = true
			if !strings.Contains(s.Label, "#") {
				t.Errorf("banner label not resolved: %q", s.Label)
			}
		}
	}
	if !found {
		t.Errorf("banner %d missing from timeseries", b.Banner.ID)
	}
	for _, dim := range []string{"banner", "placement", "country", "device"} {
		if code := analytics(t, "/api/v1/breakdown", url.Values{"dimension": {dim}}, nil); code != http.StatusOK {
			t.Errorf("breakdown %s: HTTP %d", dim, code)
		}
	}
	if code := analytics(t, "/api/v1/heatmap", url.Values{"tz": {"Europe/Moscow"}}, nil); code != http.StatusOK {
		t.Errorf("heatmap: HTTP %d", code)
	}
}

// TestForgedAndBotEventsAreIgnored checks the anti-fraud boundary.
func TestForgedAndBotEventsAreIgnored(t *testing.T) {
	waitReady(t)
	code, body := sendEvents(t, "MXxrMXxmYWtlfDF8aG9tZV90b3B8MA.AAAAAAAAAAAAAAAAAAAAAA", "v", []ev{{Type: "click", TS: time.Now().UnixMilli()}})
	if code != http.StatusAccepted || !strings.Contains(body, `"rejected":1`) {
		t.Fatalf("forged token: HTTP %d %s", code, body)
	}
	if code, _ := getBanner(t, "home_top", "RU", "Googlebot/2.1 (+http://www.google.com/bot.html)"); code != http.StatusOK {
		t.Fatalf("bots still receive a creative (served but not counted): %d", code)
	}
	if code, _ := getBanner(t, "no_such_place", "RU", browserUA); code != http.StatusNotFound {
		t.Fatalf("unknown placement: %d", code)
	}
}

// TestBannerManagementAndTargeting creates a geo-targeted banner through the
// management API and verifies it is served only to that country.
func TestBannerManagementAndTargeting(t *testing.T) {
	waitReady(t)
	body := `{"name":"e2e geo banner","image_url":"/banners/orbit-300x250.svg","click_url":"https://example.com/e2e",
		"width":300,"height":250,"weight":10000,"placements":["footer"],"countries":["KZ"]}`
	req, _ := http.NewRequest(http.MethodPost, adminURL+"/admin/v1/banners", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, rb := do(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create banner: HTTP %d %s", resp.StatusCode, rb)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rb, &created)
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/admin/v1/banners/%d", adminURL, created.ID), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		do(t, req)
	})

	// Other replicas pick the change up within REFRESH_INTERVAL; poll.
	deadline := time.Now().Add(30 * time.Second)
	hits := 0
	for time.Now().Before(deadline) && hits == 0 {
		for range 20 {
			if _, b := getBanner(t, "footer", "KZ", browserUA); b.Banner.ID == created.ID {
				hits++
			}
		}
		time.Sleep(time.Second)
	}
	if hits == 0 {
		t.Fatal("targeted banner never served to KZ")
	}
	for range 50 {
		if _, b := getBanner(t, "footer", "DE", browserUA); b.Banner.ID == created.ID {
			t.Fatal("KZ-targeted banner served to DE")
		}
	}

	req, _ = http.NewRequest(http.MethodGet, adminURL+"/admin/v1/banners", nil)
	if resp, _ := do(t, req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("management API without token: HTTP %d", resp.StatusCode)
	}
}
