//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// Chaos scenario, driven by scripts/chaos.sh in three phases:
//
//	E2E_CHAOS_PHASE=baseline  (all up)            remember current counters
//	E2E_CHAOS_PHASE=send      (ClickHouse stopped) send events; the collector must still accept them
//	E2E_CHAOS_PHASE=verify    (ClickHouse back)    every event arrives exactly once
//
// It proves that a storage outage only delays data: events wait in Kafka,
// the ingester retries the same batch with the same deduplication token and
// commits offsets only after a successful insert.
const chaosState = "chaos-state.json"

type chaosRun struct {
	BannerID int64     `json:"banner_id"`
	Start    time.Time `json:"start"`
	Before   metrics   `json:"before"`
	Sent     int       `json:"sent"`
}

func loadState(t *testing.T) chaosRun {
	t.Helper()
	raw, err := os.ReadFile(chaosState)
	if err != nil {
		t.Fatal(err)
	}
	var s chaosRun
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func saveState(t *testing.T, s chaosRun) {
	t.Helper()
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(chaosState, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChaos(t *testing.T) {
	switch os.Getenv("E2E_CHAOS_PHASE") {
	case "baseline":
		waitReady(t)
		_, b := getBanner(t, "sidebar", "RU", browserUA)
		s := chaosRun{BannerID: b.Banner.ID, Start: time.Now().Add(-time.Minute)}
		s.Before = summary(t, s.BannerID, s.Start)
		saveState(t, s)
		t.Logf("baseline for banner %d: %+v", s.BannerID, s.Before)

	case "send":
		s := loadState(t)
		for range 300 {
			code, b := getBanner(t, "sidebar", "RU", browserUA)
			if code != http.StatusOK {
				t.Fatalf("banner-service must keep serving from memory: HTTP %d", code)
			}
			if b.Banner.ID != s.BannerID {
				continue
			}
			now := time.Now().UnixMilli()
			code, body := sendEvents(t, b.Token, "chaos", []ev{{Type: "impression", TS: now, Seq: 0}, {Type: "click", TS: now, Seq: 1}})
			if code != http.StatusAccepted {
				t.Fatalf("collector must accept while storage is down: HTTP %d %s", code, body)
			}
			s.Sent++
		}
		saveState(t, s)
		t.Logf("sent %d impression+click pairs for banner %d while ClickHouse was down", s.Sent, s.BannerID)

	case "verify":
		s := loadState(t)
		waitReady(t)
		want := s.Before.Clicks + uint64(s.Sent)
		var got metrics
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
			if got = summary(t, s.BannerID, s.Start); got.Clicks >= want {
				break
			}
		}
		if got.Clicks != want || got.Impressions != s.Before.Impressions+uint64(s.Sent) {
			t.Fatalf("want exactly-once delivery: clicks %d (want %d), impressions %d (want %d)",
				got.Clicks, want, got.Impressions, s.Before.Impressions+uint64(s.Sent))
		}
		t.Logf("all %d pairs delivered exactly once after recovery", s.Sent)

	default:
		t.Skip("run via scripts/chaos.sh")
	}
}
