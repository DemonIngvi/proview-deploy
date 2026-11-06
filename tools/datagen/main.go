// Command datagen produces demo traffic for ProView.
//
//	datagen seed -days 14 -per-hour 3000   backfill history straight into ClickHouse
//	                                       (raw table; materialized views build the aggregates)
//	datagen live -rps 50 -duration 5m      simulate browsers through the public APIs
//
// The seed mode writes through the ClickHouse HTTP interface because past
// timestamps cannot be sent through the collector (it trusts client clocks
// only within minutes of the receive time, by design).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type banner struct {
	id        uint64
	placement string
	ctr       float64 // click probability per impression
	weight    float64
}

// Mirrors the demo seed of proview-banner-service (migration 0002).
var banners = []banner{
	{1, "home_top", 0.012, 3}, {2, "home_top", 0.018, 1}, {3, "sidebar", 0.025, 2},
	{4, "sidebar", 0.020, 2}, {5, "sidebar", 0.035, 3}, {6, "footer", 0.008, 1},
}

var (
	countries = []string{"RU", "RU", "RU", "RU", "KZ", "BY", "DE", "US", "AM", "UZ"}
	devices   = []string{"desktop", "desktop", "mobile", "mobile", "mobile", "tablet"}
	uas       = map[string]string{
		"desktop": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36",
		"mobile":  "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148",
		"tablet":  "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) AppleWebKit/605.1.15",
	}
)

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func pickBanner(r *rand.Rand) banner {
	total := 0.0
	for _, b := range banners {
		total += b.weight
	}
	x := r.Float64() * total
	for _, b := range banners {
		if x < b.weight {
			return b
		}
		x -= b.weight
	}
	return banners[0]
}

// seasonality: daily curve peaking in the evening (Moscow time), weekends lower.
func seasonality(t time.Time) float64 {
	msk := t.In(time.FixedZone("MSK", 3*3600))
	h := float64(msk.Hour()) + float64(msk.Minute())/60
	daily := 0.35 + 0.65*math.Pow(math.Sin(math.Pi*math.Max(0, h-6)/18), 2)
	if wd := msk.Weekday(); wd == time.Saturday || wd == time.Sunday {
		daily *= 0.75
	}
	return daily
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: datagen seed|live [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(ctx, os.Args[2:])
	case "live":
		err = live(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "datagen:", err)
		os.Exit(1)
	}
}

type row struct {
	EventID      string `json:"event_id"`
	EventType    string `json:"event_type"`
	ImpressionID string `json:"impression_id"`
	BannerID     uint64 `json:"banner_id"`
	Placement    string `json:"placement"`
	EventTime    string `json:"event_time"`
	ReceivedAt   string `json:"received_at"`
	DurationMS   uint32 `json:"duration_ms"`
	VisitorID    string `json:"visitor_id"`
	Country      string `json:"country"`
	Device       string `json:"device"`
	PageHost     string `json:"page_host"`
}

// impressionRows simulates one impression and its follow-up events.
func impressionRows(r *rand.Rand, at time.Time, seq *uint64) []row {
	b := pickBanner(r)
	dev := pick(r, devices)
	id := fmt.Sprintf("seed%d", atomic.AddUint64(seq, 1))
	base := row{ImpressionID: id, BannerID: b.id, Placement: b.placement, VisitorID: fmt.Sprintf("v%d", r.IntN(40000)),
		Country: pick(r, countries), Device: dev, PageHost: "demo.proview.local"}
	ts := func(off time.Duration) string { return at.Add(off).UTC().Format("2006-01-02 15:04:05.000") }
	out := make([]row, 0, 5)
	add := func(typ string, n int, off time.Duration, dur uint32) {
		x := base
		x.EventID, x.EventType, x.EventTime, x.ReceivedAt, x.DurationMS = fmt.Sprintf("%s:%d", id, n), typ, ts(off), ts(off), dur
		out = append(out, x)
	}
	add("impression", 0, 0, 0)
	viewP := map[string]float64{"home_top": 0.82, "sidebar": 0.7, "footer": 0.38}[b.placement]
	if r.Float64() < viewP {
		add("viewable", 1, time.Second, 0)
		add("active_time", 2, 6*time.Second, uint32(2000+r.IntN(25000)))
		hoverP := 0.22
		if dev != "desktop" {
			hoverP = 0.05 // no pointer on touch devices
		}
		if r.Float64() < hoverP {
			add("hover", 3, 3*time.Second, uint32(150+r.ExpFloat64()*900))
		}
		ctr := b.ctr * (1.6 * seasonality(at)) // evenings convert better
		if dev == "mobile" {
			ctr *= 1.2
		}
		if r.Float64() < ctr/viewP {
			add("click", 4, 4*time.Second, 0)
		}
	}
	return out
}

func seed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	chURL := fs.String("clickhouse", "http://localhost:8123", "ClickHouse HTTP URL")
	user := fs.String("user", "proview", "ClickHouse user")
	pass := fs.String("password", os.Getenv("CLICKHOUSE_PASSWORD"), "ClickHouse password (or CLICKHOUSE_PASSWORD)")
	days := fs.Int("days", 14, "days of history")
	perHour := fs.Int("per-hour", 3000, "impressions per hour at peak")
	_ = fs.Parse(args)

	r := rand.New(rand.NewPCG(42, uint64(time.Now().UnixNano())))
	end := time.Now().UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)
	var seq uint64
	total := 0
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := int(float64(*perHour) * seasonality(h.Add(30*time.Minute)) * (0.9 + 0.2*r.Float64()))
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for range n {
			for _, rw := range impressionRows(r, h.Add(time.Duration(r.Int64N(int64(time.Hour)))), &seq) {
				_ = enc.Encode(rw)
				total++
			}
		}
		q := url.Values{"query": {"INSERT INTO proview.events_raw (event_id, event_type, impression_id, banner_id, placement, event_time, received_at, duration_ms, visitor_id, country, device, page_host) FORMAT JSONEachRow"}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *chURL+"/?"+q.Encode(), &buf)
		req.SetBasicAuth(*user, *pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("insert %s: %s %s", h, resp.Status, strings.TrimSpace(string(body)))
		}
	}
	fmt.Printf("seeded %d events over %d days\n", total, *days)
	return nil
}

func live(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	web := fs.String("web", "http://localhost:8080", "site URL (proxies /v1/banner and /v1/events)")
	rps := fs.Int("rps", 50, "simulated page views per second")
	duration := fs.Duration("duration", time.Minute, "how long to run")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	var ok, failed atomic.Int64
	tick := time.NewTicker(time.Second / time.Duration(max(1, *rps)))
	defer tick.Stop()
	var wg sync.WaitGroup
	placements := []string{"home_top", "sidebar", "footer"}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			fmt.Printf("live: %d page views ok, %d failed\n", ok.Load(), failed.Load())
			return nil
		case <-tick.C:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
			dev, country := pick(r, devices), pick(r, countries)
			if err := pageView(ctx, client, *web, pick(r, placements), uas[dev], country, fmt.Sprintf("live%d", r.IntN(5000)), r); err != nil {
				failed.Add(1)
				return
			}
			ok.Add(1)
		}()
	}
}

func pageView(ctx context.Context, c *http.Client, web, placement, ua, country, visitor string, r *rand.Rand) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, web+"/v1/banner?placement="+placement, nil)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Country", country)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("banner: %s", resp.Status)
	}
	var b struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	events := []map[string]any{{"type": "impression", "ts": now, "seq": 0}}
	if r.Float64() < 0.7 {
		events = append(events, map[string]any{"type": "viewable", "ts": now + 1000, "seq": 1},
			map[string]any{"type": "active_time", "ts": now + 5000, "seq": 2, "duration_ms": 1000 + r.IntN(20000)})
		if r.Float64() < 0.2 {
			events = append(events, map[string]any{"type": "hover", "ts": now + 2000, "seq": 3, "duration_ms": 150 + r.IntN(2000)})
		}
		if r.Float64() < 0.03 {
			events = append(events, map[string]any{"type": "click", "ts": now + 3000, "seq": 4})
		}
	}
	body, _ := json.Marshal(map[string]any{"visitor_id": visitor, "page_url": web + "/", "items": []any{map[string]any{"token": b.Token, "events": events}}})
	ereq, _ := http.NewRequestWithContext(ctx, http.MethodPost, web+"/v1/events", bytes.NewReader(body))
	ereq.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	ereq.Header.Set("User-Agent", ua)
	ereq.Header.Set("X-Country", country)
	eresp, err := c.Do(ereq)
	if err != nil {
		return err
	}
	_ = eresp.Body.Close()
	if eresp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("events: %s", eresp.Status)
	}
	return nil
}
