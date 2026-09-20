// Command loadtest drives RateGuard with synthetic traffic profiles and
// reports throughput, rejection rate, and latency percentiles.
//
// It also verifies correctness under load: for every client, the number of
// admitted requests must never exceed capacity + refill_rate * elapsed. If
// the limiter ever over-admits (e.g. a race between instances), the run fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type profile struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Clients     int     `json:"clients"`
	Concurrency int     `json:"concurrency"`
	RPS         float64 `json:"target_rps"` // 0 = unpaced (as fast as possible)
	Capacity    int64   `json:"capacity"`
	Refill      float64 `json:"refill_per_sec"`
	Total       int     `json:"total_requests"`
}

func buildProfiles(total int, duration time.Duration) map[string]profile {
	sustainedRPS := 300.0
	return map[string]profile{
		"burst": {
			Name: "burst", Description: "one client, 200 workers, unpaced: a thundering herd on a single bucket",
			Clients: 1, Concurrency: 200, RPS: 0, Capacity: 200, Refill: 100, Total: total,
		},
		"sustained": {
			Name: "sustained", Description: "one client at 3x its refill rate for the full duration",
			Clients: 1, Concurrency: 64, RPS: sustainedRPS, Capacity: 200, Refill: 100,
			Total: int(sustainedRPS * duration.Seconds()),
		},
		"multi-client": {
			Name: "multi-client", Description: "100 clients with small buckets, unpaced: many independent buckets under load",
			Clients: 100, Concurrency: 100, RPS: 0, Capacity: 20, Refill: 10, Total: total,
		},
	}
}

var profileOrder = []string{"burst", "sustained", "multi-client"}

type sample struct {
	start, end time.Duration // offsets from run start
	client     int32
	status     int16 // 0 = transport error
}

// Result is the machine-readable summary of one profile run.
type Result struct {
	Profile      profile            `json:"profile"`
	Requests     int                `json:"requests"`
	Allowed      int                `json:"allowed"`
	Denied       int                `json:"denied"`
	Errors       int                `json:"errors"`
	RejectPct    float64            `json:"reject_pct"`
	ElapsedSec   float64            `json:"elapsed_sec"`
	Throughput   float64            `json:"throughput_rps"`
	LatencyMs    map[string]float64 `json:"latency_ms"`
	MaxPermitted float64            `json:"max_permitted_by_policy"`
	QuotaUsePct  float64            `json:"quota_utilisation_pct"`
	Violations   int                `json:"clients_over_quota"`
	Instances    map[string]int     `json:"requests_by_instance"`
}

func main() {
	var (
		targets    = flag.String("targets", "http://localhost:8080", "comma-separated RateGuard base URLs (round-robin)")
		profileSel = flag.String("profile", "all", "burst | sustained | multi-client | all")
		total      = flag.Int("total", 5000, "requests per unpaced profile")
		duration   = flag.Duration("duration", 10*time.Second, "length of the sustained profile")
		adminToken = flag.String("admin-token", os.Getenv("ADMIN_TOKEN"), "bearer token for policy setup")
		out        = flag.String("out", "results/loadtest.json", "JSON results path ('' to disable)")
	)
	flag.Parse()

	urls := splitTrim(*targets)
	if len(urls) == 0 {
		fatalf("no targets")
	}
	profiles := buildProfiles(*total, *duration)

	var run []string
	if *profileSel == "all" {
		run = profileOrder
	} else if _, ok := profiles[*profileSel]; ok {
		run = []string{*profileSel}
	} else {
		fatalf("unknown profile %q", *profileSel)
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	runID := time.Now().Unix()
	var results []Result
	failed := false
	for _, name := range run {
		p := profiles[name]
		ids := make([]string, p.Clients)
		for i := range ids {
			ids[i] = fmt.Sprintf("lt-%s-%d-%d", p.Name, runID, i)
		}
		if err := setupPolicies(client, urls[0], *adminToken, ids, p); err != nil {
			fatalf("setup %s: %v", p.Name, err)
		}
		fmt.Printf("\n▶ %s: %s\n  %d requests, %d clients, %d workers, capacity=%d refill=%.0f/s, targets=%d\n",
			p.Name, p.Description, p.Total, p.Clients, p.Concurrency, p.Capacity, p.Refill, len(urls))

		res := execute(client, urls, ids, p)
		results = append(results, res)
		printResult(res)
		if res.Violations > 0 || res.Errors > 0 {
			failed = true
		}
	}

	printSummary(results)

	if *out != "" {
		if err := writeJSON(*out, results); err != nil {
			fatalf("write results: %v", err)
		}
		fmt.Printf("\nresults written to %s\n", *out)
	}
	if failed {
		fmt.Println("\nFAIL: over-admission or request errors detected")
		os.Exit(1)
	}
	fmt.Println("\nPASS: no client exceeded its quota")
}

func setupPolicies(c *http.Client, base, token string, ids []string, p profile) error {
	body, _ := json.Marshal(map[string]any{"capacity": p.Capacity, "refill_per_sec": p.Refill})
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
				base+"/v1/policies/"+id, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := c.Do(req)
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				firstErr.CompareAndSwap(nil, fmt.Errorf("PUT policy: HTTP %d", resp.StatusCode))
			}
		}(id)
	}
	wg.Wait()
	if v := firstErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func execute(c *http.Client, urls, ids []string, p profile) Result {
	n := p.Total
	samples := make([]sample, n) // each index written by exactly one goroutine
	bodies := make([][]byte, len(ids))
	for i, id := range ids {
		bodies[i], _ = json.Marshal(map[string]any{"client_id": id})
	}

	var next atomic.Int64
	instMu := sync.Mutex{}
	instances := map[string]int{}

	t0 := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < p.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[string]int{}
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					break
				}
				if p.RPS > 0 { // open-loop pacing: request i is due at t0 + i/rps
					due := t0.Add(time.Duration(float64(i) / p.RPS * float64(time.Second)))
					time.Sleep(time.Until(due))
				}
				cl := i % len(ids)
				url := urls[i%len(urls)] + "/v1/check"

				start := time.Now()
				status, inst := doCheck(c, url, bodies[cl])
				end := time.Now()

				samples[i] = sample{start: start.Sub(t0), end: end.Sub(t0), client: int32(cl), status: int16(status)}
				if inst != "" {
					local[inst]++
				}
			}
			instMu.Lock()
			for k, v := range local {
				instances[k] += v
			}
			instMu.Unlock()
		}()
	}
	wg.Wait()

	return analyse(p, samples, instances)
}

func doCheck(c *http.Client, url string, body []byte) (status int, instance string) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-RateGuard-Instance")
}

func analyse(p profile, samples []sample, instances map[string]int) Result {
	res := Result{Profile: p, Requests: len(samples), Instances: instances}

	type window struct {
		first, last time.Duration
		allowed     int
		seen        bool
	}
	wins := make([]window, p.Clients)
	lat := make([]time.Duration, 0, len(samples))
	var firstStart, lastEnd time.Duration = math.MaxInt64, 0

	for _, s := range samples {
		if s.start < firstStart {
			firstStart = s.start
		}
		if s.end > lastEnd {
			lastEnd = s.end
		}
		switch s.status {
		case http.StatusOK:
			res.Allowed++
			wins[s.client].allowed++
		case http.StatusTooManyRequests:
			res.Denied++
		default:
			res.Errors++
			continue // don't pollute latency with timeouts/failures
		}
		lat = append(lat, s.end-s.start)

		w := &wins[s.client]
		if !w.seen || s.start < w.first {
			w.first = s.start
		}
		if s.end > w.last {
			w.last = s.end
		}
		w.seen = true
	}

	elapsed := lastEnd - firstStart
	res.ElapsedSec = elapsed.Seconds()
	if res.ElapsedSec > 0 {
		res.Throughput = float64(res.Requests) / res.ElapsedSec
	}
	if res.Requests > 0 {
		res.RejectPct = 100 * float64(res.Denied) / float64(res.Requests)
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	res.LatencyMs = map[string]float64{
		"p50": ms(pct(lat, 0.50)), "p95": ms(pct(lat, 0.95)),
		"p99": ms(pct(lat, 0.99)), "max": ms(pct(lat, 1.0)),
	}

	// Quota check. A bucket can hand out at most capacity + refill*T tokens
	// over any window of length T. The client-observed window (first send to
	// last response) contains the server-side window, so it is a safe upper
	// bound; +1 absorbs the fractional token at the boundary.
	for _, w := range wins {
		if !w.seen {
			continue
		}
		bound := float64(p.Capacity) + p.Refill*(w.last-w.first).Seconds()
		res.MaxPermitted += bound
		if float64(w.allowed) > math.Floor(bound)+1 {
			res.Violations++
		}
	}
	if res.MaxPermitted > 0 {
		res.QuotaUsePct = 100 * float64(res.Allowed) / res.MaxPermitted
	}
	return res
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*100) / 100 }

func printResult(r Result) {
	fmt.Printf("  elapsed      %.2fs   throughput %.0f req/s\n", r.ElapsedSec, r.Throughput)
	fmt.Printf("  allowed      %d   denied %d (%.1f%%)   errors %d\n", r.Allowed, r.Denied, r.RejectPct, r.Errors)
	fmt.Printf("  latency      p50 %.2fms   p95 %.2fms   p99 %.2fms   max %.2fms\n",
		r.LatencyMs["p50"], r.LatencyMs["p95"], r.LatencyMs["p99"], r.LatencyMs["max"])
	fmt.Printf("  quota check  admitted %d of max %.0f permitted (%.1f%%), clients over quota: %d\n",
		r.Allowed, r.MaxPermitted, r.QuotaUsePct, r.Violations)
	if len(r.Instances) > 0 {
		keys := make([]string, 0, len(r.Instances))
		for k := range r.Instances {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%d", k, r.Instances[k])
		}
		fmt.Printf("  instances    %s\n", strings.Join(parts, "  "))
	}
}

func printSummary(rs []Result) {
	fmt.Println("\n" + strings.Repeat("═", 96))
	fmt.Printf("%-14s %8s %9s %8s %8s %9s %8s %8s %8s\n",
		"PROFILE", "REQS", "RPS", "ALLOWED", "REJECT%", "P50 ms", "P95 ms", "P99 ms", "OVER")
	fmt.Println(strings.Repeat("─", 96))
	total := 0
	for _, r := range rs {
		total += r.Requests
		fmt.Printf("%-14s %8d %9.0f %8d %7.1f%% %9.2f %8.2f %8.2f %8d\n",
			r.Profile.Name, r.Requests, r.Throughput, r.Allowed, r.RejectPct,
			r.LatencyMs["p50"], r.LatencyMs["p95"], r.LatencyMs["p99"], r.Violations)
	}
	fmt.Println(strings.Repeat("═", 96))
	fmt.Printf("total requests: %d\n", total)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+f+"\n", a...)
	os.Exit(2)
}
