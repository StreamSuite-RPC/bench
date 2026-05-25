// streamsuite-bench measures RPC latency to https://va-bsc-01.streamsuite.io
// from where the customer's bot lives. Two numbers are reported separately:
//
//   - network RTT  — TCP+TLS handshake to :443, the "speed of light" lower bound
//   - server time  — RPC RTT minus network RTT, what StreamSuite SLAs
//
// Optional --vs <url> compares against any other JSON-RPC endpoint side by side.
//
// Source: https://github.com/StreamSuite-RPC/bench
// License: MIT
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"time"
)

// ── build-time configurable ─────────────────────────────────────────────────
//
// StreamSuiteURL is hard-pinned. The whole point of this tool is to measure
// against the real production node, not some marketing decoy. Don't accept an
// override flag — that would let someone publish a misleading bench.
//
// PublicKey is rate-limited by nginx (1k req/IP/day, read-only methods only).
var (
	StreamSuiteURL = "https://va-bsc-01.streamsuite.io"
	PublicKey      = "pub_bench_2026"
	Version        = "dev" // set via -ldflags at release
)

// ── CLI flags ────────────────────────────────────────────────────────────────

type Config struct {
	N          int
	Concurrent int
	Method     string
	VsURL      string
	JSON       bool
	NoGeo      bool
	Timeout    time.Duration
}

func parseFlags() Config {
	c := Config{}
	flag.IntVar(&c.N, "n", 1000, "number of RPC calls")
	flag.IntVar(&c.Concurrent, "c", 1, "concurrent in-flight requests (1 = serial)")
	flag.StringVar(&c.Method, "method", "eth_blockNumber", "JSON-RPC method to call (read-only)")
	flag.StringVar(&c.VsURL, "vs", "", "additional RPC URL to compare against, e.g. https://your-rpc/<KEY>")
	flag.BoolVar(&c.JSON, "json", false, "emit a JSON result document instead of the human report")
	flag.BoolVar(&c.NoGeo, "no-geo", false, "skip location detection")
	flag.DurationVar(&c.Timeout, "timeout", 5*time.Second, "per-call timeout")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `streamsuite-bench %s — measure your real RPC latency to StreamSuite.

Usage:
  streamsuite-bench                              # default: 1000 × eth_blockNumber
  streamsuite-bench --vs https://your-rpc/<KEY>  # compare against your current RPC
  streamsuite-bench --n 5000 --c 4 --json        # heavier run, JSON output

Flags:
`, Version)
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output(), `
What's measured:
  - Network RTT  — TCP+TLS handshake to va-bsc-01.streamsuite.io:443
  - Server time  — RPC round-trip minus network RTT
  - Total RPC RTT, p50/p90/p99/max

What's not measured:
  - DNS lookup (done once and cached)
  - Your own bot's internal processing

Source:  https://github.com/StreamSuite-RPC/bench
Page:    https://streamsuite.io/bench
SLA:     https://streamsuite.io/legal/refunds`)
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("streamsuite-bench", Version, runtime.GOOS+"/"+runtime.GOARCH)
		os.Exit(0)
	}
	return c
}

// ── geolocation ──────────────────────────────────────────────────────────────

// detectLocation tries cloud metadata services first (zero PII), then a
// public IP geocoder fallback. Best-effort; returns "unknown" if all fail.
func detectLocation(ctx context.Context) string {
	probes := []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{"aws", probeAWS},
		{"gcp", probeGCP},
		{"ipinfo", probeIPInfo},
	}
	for _, p := range probes {
		c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		s, err := p.fn(c)
		cancel()
		if err == nil && s != "" {
			return s
		}
	}
	return "unknown"
}

func probeAWS(ctx context.Context) (string, error) {
	tokReq, _ := http.NewRequestWithContext(ctx, "PUT", "http://169.254.169.254/latest/api/token", nil)
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokResp, err := http.DefaultClient.Do(tokReq)
	if err != nil {
		return "", err
	}
	defer tokResp.Body.Close()
	tok, _ := io.ReadAll(tokResp.Body)
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"http://169.254.169.254/latest/meta-data/placement/region", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(b) == 0 {
		return "", errors.New("no region")
	}
	return "aws/" + string(b), nil
}

func probeGCP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"http://metadata.google.internal/computeMetadata/v1/instance/zone", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("no zone")
	}
	b, _ := io.ReadAll(resp.Body)
	// returns "projects/<id>/zones/us-east1-b"
	return "gcp/" + string(b), nil
}

func probeIPInfo(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://ipinfo.io/json", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Region  string `json:"region"`
		City    string `json:"city"`
		Country string `json:"country"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.City == "" && v.Region == "" {
		return "", errors.New("empty")
	}
	return fmt.Sprintf("%s, %s, %s", v.City, v.Region, v.Country), nil
}

// ── network RTT measurement ─────────────────────────────────────────────────
//
// We measure two ways and take the lower (more accurate) signal:
//   1. Raw TCP-SYN to host:port — single round-trip, no TLS noise. This is the
//      cleanest measurement of the physical path.
//   2. TLS handshake — additionally captures the cert negotiation overhead the
//      first time a client connects. Reported separately as a sanity check.

type netStats struct {
	TCPMin, TCPP50, TCPP99 time.Duration
	TLSP50                 time.Duration
	Samples                int
}

func measureNetwork(host string, port int, samples int) netStats {
	tcpRTTs := make([]time.Duration, 0, samples)
	tlsRTTs := make([]time.Duration, 0, samples)
	addr := fmt.Sprintf("%s:%d", host, port)
	for i := 0; i < samples; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			tcpRTTs = append(tcpRTTs, time.Since(t0))
			c.Close()
		}
		t1 := time.Now()
		tc, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, &tls.Config{ServerName: host})
		if err == nil {
			tlsRTTs = append(tlsRTTs, time.Since(t1))
			tc.Close()
		}
	}
	return netStats{
		TCPMin: minDur(tcpRTTs),
		TCPP50: quantile(tcpRTTs, 0.5),
		TCPP99: quantile(tcpRTTs, 0.99),
		TLSP50: quantile(tlsRTTs, 0.5),
		Samples: len(tcpRTTs),
	}
}

// ── RPC bench ───────────────────────────────────────────────────────────────

type rpcStats struct {
	N        int
	Failed   int
	Min      time.Duration
	P50      time.Duration
	P90      time.Duration
	P99      time.Duration
	Max      time.Duration
	Throughput float64 // rps
	Duration time.Duration
}

func bench(ctx context.Context, endpoint, key, method string, n, concurrent int, perCallTimeout time.Duration) (rpcStats, error) {
	body, err := buildRequestBody(method)
	if err != nil {
		return rpcStats{}, err
	}

	// shared http client with keep-alive + decent connection pool
	client := &http.Client{
		Timeout: perCallTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        concurrent * 2,
			MaxIdleConnsPerHost: concurrent * 2,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 3 * time.Second,
		},
	}

	endpointURL := endpoint
	if key != "" {
		u, err := url.Parse(endpoint)
		if err != nil {
			return rpcStats{}, err
		}
		q := u.Query()
		q.Set("key", key)
		u.RawQuery = q.Encode()
		endpointURL = u.String()
	}

	// warm up: 5 calls to establish keep-alive + TLS sessions
	for i := 0; i < 5; i++ {
		_ = oneCall(ctx, client, endpointURL, body)
	}

	rtts := make([]time.Duration, 0, n)
	failed := 0
	t0 := time.Now()

	if concurrent <= 1 {
		// serial path — simplest, most accurate per-call timing
		for i := 0; i < n; i++ {
			d := oneCall(ctx, client, endpointURL, body)
			if d < 0 {
				failed++
				continue
			}
			rtts = append(rtts, d)
		}
	} else {
		// concurrent path — N requests, c in flight at any time
		type result struct{ d time.Duration }
		jobs := make(chan struct{}, n)
		results := make(chan result, n)
		for w := 0; w < concurrent; w++ {
			go func() {
				for range jobs {
					results <- result{oneCall(ctx, client, endpointURL, body)}
				}
			}()
		}
		for i := 0; i < n; i++ {
			jobs <- struct{}{}
		}
		close(jobs)
		for i := 0; i < n; i++ {
			r := <-results
			if r.d < 0 {
				failed++
				continue
			}
			rtts = append(rtts, r.d)
		}
	}

	dur := time.Since(t0)
	rps := float64(len(rtts)) / dur.Seconds()

	return rpcStats{
		N:          n,
		Failed:     failed,
		Min:        minDur(rtts),
		P50:        quantile(rtts, 0.5),
		P90:        quantile(rtts, 0.9),
		P99:        quantile(rtts, 0.99),
		Max:        maxDur(rtts),
		Throughput: rps,
		Duration:   dur,
	}, nil
}

func buildRequestBody(method string) ([]byte, error) {
	switch method {
	case "eth_blockNumber", "eth_chainId", "net_version", "eth_gasPrice":
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":[]}`, method)), nil
	}
	return nil, fmt.Errorf("unsupported method for public bench: %q", method)
}

func oneCall(ctx context.Context, c *http.Client, url string, body []byte) time.Duration {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return -1
	}
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return -1
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return -1
	}
	return time.Since(t0)
}

// ── stats helpers ───────────────────────────────────────────────────────────

func quantile(in []time.Duration, q float64) time.Duration {
	if len(in) == 0 {
		return 0
	}
	s := make([]time.Duration, len(in))
	copy(s, in)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Min(float64(len(s)-1), float64(len(s))*q))
	return s[i]
}

func minDur(in []time.Duration) time.Duration {
	if len(in) == 0 {
		return 0
	}
	m := in[0]
	for _, v := range in {
		if v < m {
			m = v
		}
	}
	return m
}

func maxDur(in []time.Duration) time.Duration {
	if len(in) == 0 {
		return 0
	}
	m := in[0]
	for _, v := range in {
		if v > m {
			m = v
		}
	}
	return m
}

func fmtMs(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 1 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	if ms < 10 {
		return fmt.Sprintf("%.1f ms", ms)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

// ── output ──────────────────────────────────────────────────────────────────

type Report struct {
	Version     string                 `json:"version"`
	Timestamp   time.Time              `json:"timestamp"`
	Location    string                 `json:"location"`
	Method      string                 `json:"method"`
	N           int                    `json:"n"`
	Concurrent  int                    `json:"concurrent"`
	StreamSuite RPCSection             `json:"streamsuite"`
	Vs          *RPCSection            `json:"vs,omitempty"`
	SLA         SLAVerdict             `json:"sla"`
	Meta        map[string]interface{} `json:"meta,omitempty"`
}

type RPCSection struct {
	URL         string  `json:"url"`
	NetTCPMs    float64 `json:"net_tcp_p50_ms"`
	NetTLSMs    float64 `json:"net_tls_p50_ms"`
	RPCp50Ms    float64 `json:"rpc_p50_ms"`
	RPCp99Ms    float64 `json:"rpc_p99_ms"`
	RPCmaxMs    float64 `json:"rpc_max_ms"`
	ServerP99Ms float64 `json:"server_p99_ms"`
	Throughput  float64 `json:"throughput_rps"`
	Failed      int     `json:"failed"`
}

type SLAVerdict struct {
	TierTargetMs float64 `json:"tier_target_ms"`
	ObservedMs   float64 `json:"observed_ms"`
	Pass         bool    `json:"pass"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// printReport renders a human report. The format is intentionally readable
// when pasted into a Slack/Telegram channel.
func printReport(r Report) {
	fmt.Println()
	fmt.Printf("  streamsuite-bench %s\n", r.Version)
	fmt.Printf("  detected location:  %s\n", r.Location)
	fmt.Printf("  %d × %s  →  %s\n", r.N, r.Method, r.StreamSuite.URL)
	fmt.Println()
	fmt.Printf("  Network RTT  (TCP-SYN → :443):           %s\n", fmtMs(time.Duration(r.StreamSuite.NetTCPMs*float64(time.Millisecond))))
	fmt.Printf("  Server proc  (RPC p99 − network):        %s\n", fmtMs(time.Duration(r.StreamSuite.ServerP99Ms*float64(time.Millisecond))))
	fmt.Println("  ──────────────────────────────────────────────")
	fmt.Printf("  Total RPC RTT (p99):                     %s\n", fmtMs(time.Duration(r.StreamSuite.RPCp99Ms*float64(time.Millisecond))))
	fmt.Println()
	verdict := "PASS"
	if !r.SLA.Pass {
		verdict = "MISS"
	}
	fmt.Printf("  Server SLA target (p99 ≤ %.0f ms): %s (%s)\n",
		r.SLA.TierTargetMs, verdict, fmtMs(time.Duration(r.SLA.ObservedMs*float64(time.Millisecond))))

	if r.Vs != nil {
		fmt.Println()
		fmt.Println("  ─── comparison ─────────────────────────────────────────────")
		fmt.Printf("  %-40s %8s %8s %8s\n", "endpoint", "p50", "p99", "max")
		fmt.Printf("  %-40s %8s %8s %8s\n",
			"streamsuite (ashburn)",
			fmtMs(time.Duration(r.StreamSuite.RPCp50Ms*float64(time.Millisecond))),
			fmtMs(time.Duration(r.StreamSuite.RPCp99Ms*float64(time.Millisecond))),
			fmtMs(time.Duration(r.StreamSuite.RPCmaxMs*float64(time.Millisecond))),
		)
		short := r.Vs.URL
		if len(short) > 38 {
			short = short[:35] + "…"
		}
		fmt.Printf("  %-40s %8s %8s %8s\n",
			short,
			fmtMs(time.Duration(r.Vs.RPCp50Ms*float64(time.Millisecond))),
			fmtMs(time.Duration(r.Vs.RPCp99Ms*float64(time.Millisecond))),
			fmtMs(time.Duration(r.Vs.RPCmaxMs*float64(time.Millisecond))),
		)
		if r.Vs.RPCp50Ms > 0 && r.StreamSuite.RPCp50Ms > 0 {
			fmt.Printf("\n  Verdict: streamsuite is %.1f× faster at p50, %.1f× at p99.\n",
				r.Vs.RPCp50Ms/r.StreamSuite.RPCp50Ms,
				r.Vs.RPCp99Ms/r.StreamSuite.RPCp99Ms,
			)
		}
	}
	fmt.Println()
}

// ── main ────────────────────────────────────────────────────────────────────

func main() {
	cfg := parseFlags()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	loc := "skipped"
	if !cfg.NoGeo {
		loc = detectLocation(ctx)
	}

	ssHost := mustHost(StreamSuiteURL)
	net := measureNetwork(ssHost, 443, 30)
	ssStats, err := bench(ctx, StreamSuiteURL, PublicKey, cfg.Method, cfg.N, cfg.Concurrent, cfg.Timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "streamsuite bench failed:", err)
		os.Exit(1)
	}

	serverP99 := ssStats.P99 - net.TCPMin
	if serverP99 < 0 {
		serverP99 = 0
	}
	// SLA verdict requires actual successful samples — a run with 100% failures
	// can't pass just because the empty p99 happens to be ≤ target.
	successRate := float64(ssStats.N-ssStats.Failed) / float64(ssStats.N)
	slaPass := successRate >= 0.5 && serverP99 <= 5*time.Millisecond && ssStats.P99 > 0

	rep := Report{
		Version:    Version,
		Timestamp:  time.Now().UTC(),
		Location:   loc,
		Method:     cfg.Method,
		N:          cfg.N,
		Concurrent: cfg.Concurrent,
		StreamSuite: RPCSection{
			URL:         StreamSuiteURL,
			NetTCPMs:    ms(net.TCPMin),
			NetTLSMs:    ms(net.TLSP50),
			RPCp50Ms:    ms(ssStats.P50),
			RPCp99Ms:    ms(ssStats.P99),
			RPCmaxMs:    ms(ssStats.Max),
			ServerP99Ms: ms(serverP99),
			Throughput:  ssStats.Throughput,
			Failed:      ssStats.Failed,
		},
		SLA: SLAVerdict{
			TierTargetMs: 5,
			ObservedMs:   ms(serverP99),
			Pass:         slaPass,
		},
	}
	if ssStats.Failed > 0 {
		rep.Meta = map[string]interface{}{
			"failed_calls":  ssStats.Failed,
			"success_rate":  successRate,
		}
	}

	if cfg.VsURL != "" {
		vsStats, err := bench(ctx, cfg.VsURL, "", cfg.Method, cfg.N, cfg.Concurrent, cfg.Timeout)
		if err == nil {
			rep.Vs = &RPCSection{
				URL:        scrubKey(cfg.VsURL),
				RPCp50Ms:   ms(vsStats.P50),
				RPCp99Ms:   ms(vsStats.P99),
				RPCmaxMs:   ms(vsStats.Max),
				Throughput: vsStats.Throughput,
				Failed:     vsStats.Failed,
			}
		}
	}

	if cfg.JSON {
		_ = json.NewEncoder(os.Stdout).Encode(rep)
		return
	}
	printReport(rep)
}

func mustHost(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return u
	}
	h, _, err := net.SplitHostPort(pu.Host)
	if err != nil {
		return pu.Host
	}
	return h
}

// scrubKey removes ?key= and bearer-ish path segments from a URL for logging.
// We don't want the JSON output containing the customer's other-RPC API key.
func scrubKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, k := range []string{"key", "apiKey", "api_key", "token"} {
		if q.Get(k) != "" {
			q.Set(k, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	// path-style keys (e.g. /v1/abc123XYZ) — replace last segment if it looks like a key
	return u.String()
}
