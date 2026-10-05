// Command loadtest ramps up load against a locally running wallet-go API
// until it breaks, then checks that the ledger is still consistent.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const fundingCents int64 = 1_000_000_000

type client struct {
	base string
	key  string
	http *http.Client
}

func newClient(base, key string, maxWorkers int) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		key:  key,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        maxWorkers,
				MaxIdleConnsPerHost: maxWorkers,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

type target struct {
	method  string
	path    string
	body    []byte
	idemKey string
}

func (c *client) newRequest(t target) (*http.Request, error) {
	var body io.Reader
	if t.body != nil {
		body = bytes.NewReader(t.body)
	}
	req, err := http.NewRequestWithContext(context.Background(), t.method, c.base+t.path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.key)
	if t.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.idemKey != "" {
		req.Header.Set("Idempotency-Key", t.idemKey)
	}
	return req, nil
}

// send performs the request and discards the body so connections are reused.
func (c *client) send(t target) (int, error) {
	req, err := c.newRequest(t)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// call is like send but returns the response body. Used for setup and checks.
func (c *client) call(t target) (int, []byte, error) {
	req, err := c.newRequest(t)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (c *client) createAccount(owner string) (string, error) {
	body, _ := json.Marshal(map[string]string{"owner_id": owner})
	status, b, err := c.call(target{method: http.MethodPost, path: "/accounts", body: body})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("create account: status %d: %s", status, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *client) deposit(id string, cents int64, key string) error {
	body, _ := json.Marshal(map[string]int64{"amount_cents": cents})
	status, b, err := c.call(target{method: http.MethodPost, path: "/accounts/" + id + "/deposits", body: body, idemKey: key})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("deposit: status %d: %s", status, b)
	}
	return nil
}

func (c *client) balance(id string) (int64, error) {
	status, b, err := c.call(target{method: http.MethodGet, path: "/accounts/" + id + "/balance"})
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("balance: status %d: %s", status, b)
	}
	var out struct {
		BalanceCents int64 `json:"balance_cents"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return 0, err
	}
	return out.BalanceCents, nil
}

type stageResult struct {
	workers   int
	elapsed   time.Duration
	latencies []time.Duration
	statuses  map[int]int
	errs      map[string]int
}

// classify sorts a transport-level error (no HTTP response at all) into a
// coarse category. These are distinct from application errors (4xx/5xx,
// tracked in statuses): a connection_refused burst during a traffic ramp is
// often a client/OS limit (e.g. the accept queue), not the API saturating.
func classify(err error) string {
	var ne net.Error
	msg := err.Error()
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "client_timeout"
	case strings.Contains(msg, "refused"):
		return "connection_refused"
	case errors.Is(err, io.EOF), strings.Contains(msg, "reset"), strings.Contains(msg, "forcibly closed"):
		return "connection_reset"
	default:
		return "transport_error"
	}
}

// runStage drives workers concurrent request loops for duration d. Workers
// are not all started at once: their first request is staggered linearly
// over ramp (capped to half of d), so a stage does not open hundreds of TCP
// connections in the same instant. seed makes the sequence of accounts/pairs
// each worker picks deterministic and reproducible across runs.
func runStage(c *client, next func(*rand.Rand) target, workers int, d, ramp time.Duration, seed int64) stageResult {
	if ramp > d/2 {
		ramp = d / 2
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		res = stageResult{workers: workers, statuses: map[int]int{}, errs: map[string]int{}}
	)
	begin := time.Now()
	deadline := begin.Add(d)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		startDelay := time.Duration(0)
		if workers > 1 && ramp > 0 {
			startDelay = time.Duration(int64(w) * int64(ramp) / int64(workers))
		}
		go func(w int, startDelay time.Duration) {
			defer wg.Done()
			time.Sleep(startDelay)

			rng := rand.New(rand.NewPCG(uint64(seed), uint64(w))) //nolint:gosec // load generator does not need crypto randomness
			var lat []time.Duration
			statuses := map[int]int{}
			errs := map[string]int{}
			for time.Now().Before(deadline) {
				t := next(rng)
				t0 := time.Now()
				status, err := c.send(t)
				lat = append(lat, time.Since(t0))
				if err != nil {
					errs[classify(err)]++
				} else {
					statuses[status]++
				}
			}
			mu.Lock()
			defer mu.Unlock()
			res.latencies = append(res.latencies, lat...)
			for k, v := range statuses {
				res.statuses[k] += v
			}
			for k, v := range errs {
				res.errs[k] += v
			}
		}(w, startDelay)
	}
	wg.Wait()
	res.elapsed = time.Since(begin)
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	return res
}

func (r stageResult) percentile(p float64) time.Duration {
	if len(r.latencies) == 0 {
		return 0
	}
	i := int(float64(len(r.latencies)) * p)
	if i >= len(r.latencies) {
		i = len(r.latencies) - 1
	}
	return r.latencies[i]
}

// appFailureRatio is the share of completed requests (got an HTTP response)
// that were not 2xx. This is the API itself reporting trouble.
func (r stageResult) appFailureRatio() float64 {
	total := len(r.latencies)
	if total == 0 {
		return 0
	}
	bad := 0
	for status, n := range r.statuses {
		if status < 200 || status >= 300 {
			bad += n
		}
	}
	return float64(bad) / float64(total)
}

// connFailureRatio is the share of attempts that never got an HTTP response
// at all (refused, reset, timed out). Treat this as ambiguous: it can mean
// the server stopped accepting connections, or that the client/OS hit a
// limit of its own (see docs/loadtest.md).
func (r stageResult) connFailureRatio() float64 {
	total := len(r.latencies)
	if total == 0 {
		return 0
	}
	bad := 0
	for _, n := range r.errs {
		bad += n
	}
	return float64(bad) / float64(total)
}

func (r stageResult) report() {
	total := len(r.latencies)
	rps := float64(total) / r.elapsed.Seconds()
	round := func(d time.Duration) time.Duration { return d.Round(100 * time.Microsecond) }
	fmt.Printf("workers=%-4d rps=%-7.0f app_fail=%5.1f%% conn_fail=%5.1f%%  p50=%-8s p95=%-8s p99=%-8s max=%-8s status=%v errs=%v\n",
		r.workers, rps, r.appFailureRatio()*100, r.connFailureRatio()*100,
		round(r.percentile(0.50)), round(r.percentile(0.95)), round(r.percentile(0.99)), round(r.percentile(1)),
		r.statuses, r.errs)
}

// verdict is the break/no-break call for one stage, with the reason kept
// specific to which signal tripped: latency, the API's own error responses,
// or connection-level failures (which deserve more skepticism).
type verdict struct {
	broke  bool
	reason string
}

func evaluateStage(r stageResult, errLimit float64, p99Limit time.Duration) verdict {
	if p99 := r.percentile(0.99); p99 > p99Limit {
		return verdict{true, fmt.Sprintf("p99 latency %s is above %s", p99.Round(time.Millisecond), p99Limit)}
	}
	if af := r.appFailureRatio(); af > errLimit {
		return verdict{true, fmt.Sprintf("application failure ratio %.1f%% (non-2xx responses) is above %.1f%%", af*100, errLimit*100)}
	}
	if cf := r.connFailureRatio(); cf > errLimit {
		return verdict{true, fmt.Sprintf("connection failure ratio %.1f%% (refused/reset/timeout) is above %.1f%% -- this can be a client/OS limit rather than API saturation, see docs/loadtest.md", cf*100, errLimit*100)}
	}
	return verdict{}
}

func isLocal(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	return fmt.Errorf("refusing to load test %q: only localhost is allowed", u.Hostname())
}

// --- bench report (docs/bench/<scenario>-<seed>.json) ---

type benchReport struct {
	Timestamp   time.Time         `json:"timestamp"`
	Scenario    string            `json:"scenario"`
	Seed        int64             `json:"seed"`
	Params      benchParams       `json:"params"`
	Stages      []benchStage      `json:"stages"`
	BrokeAt     int               `json:"broke_at_workers,omitempty"`
	BrokeReason string            `json:"broke_reason,omitempty"`
	RecoveredIn string            `json:"recovered_in,omitempty"`
	Consistency *benchConsistency `json:"consistency,omitempty"`
}

type benchParams struct {
	Base          string  `json:"base"`
	StageDuration string  `json:"stage_duration"`
	RampUp        string  `json:"ramp_up"`
	StartWorkers  int     `json:"start_workers"`
	MaxWorkers    int     `json:"max_workers"`
	Accounts      int     `json:"accounts"`
	ErrLimit      float64 `json:"err_limit"`
	P99Limit      string  `json:"p99_limit"`
}

type benchStage struct {
	Workers          int            `json:"workers"`
	DurationMS       int64          `json:"duration_ms"`
	Requests         int            `json:"requests"`
	RPS              float64        `json:"rps"`
	P50Ms            float64        `json:"p50_ms"`
	P95Ms            float64        `json:"p95_ms"`
	P99Ms            float64        `json:"p99_ms"`
	MaxMs            float64        `json:"max_ms"`
	Statuses         map[string]int `json:"statuses"`
	TransportErrors  map[string]int `json:"transport_errors"`
	AppFailureRatio  float64        `json:"app_failure_ratio"`
	ConnFailureRatio float64        `json:"conn_failure_ratio"`
	Broke            bool           `json:"broke"`
	BrokeReason      string         `json:"broke_reason,omitempty"`
}

type benchConsistency struct {
	Passed        bool  `json:"passed"`
	ActualCents   int64 `json:"actual_cents"`
	ExpectedCents int64 `json:"expected_cents"`
	DiffCents     int64 `json:"diff_cents"`
}

func toBenchStage(r stageResult, v verdict) benchStage {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	statuses := make(map[string]int, len(r.statuses))
	for status, n := range r.statuses {
		statuses[fmt.Sprintf("%d", status)] = n
	}
	return benchStage{
		Workers:          r.workers,
		DurationMS:       r.elapsed.Milliseconds(),
		Requests:         len(r.latencies),
		RPS:              float64(len(r.latencies)) / r.elapsed.Seconds(),
		P50Ms:            ms(r.percentile(0.50)),
		P95Ms:            ms(r.percentile(0.95)),
		P99Ms:            ms(r.percentile(0.99)),
		MaxMs:            ms(r.percentile(1)),
		Statuses:         statuses,
		TransportErrors:  r.errs,
		AppFailureRatio:  r.appFailureRatio(),
		ConnFailureRatio: r.connFailureRatio(),
		Broke:            v.broke,
		BrokeReason:      v.reason,
	}
}

// writeBenchReport saves one JSON file per execution under docs/bench/,
// named after the scenario and seed so a run is identifiable and, modulo
// live timing, repeatable (go run ./cmd/loadtest ... -seed <seed>).
func writeBenchReport(rep benchReport) (string, error) {
	dir := filepath.Join("docs", "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-seed%d-%s.json", rep.Scenario, rep.Seed, rep.Timestamp.Format("20060102-150405")))
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // bench report is not sensitive
		return "", err
	}
	return path, nil
}

func main() {
	var (
		base      = flag.String("base", "http://localhost:8080", "API base URL (localhost only)")
		key       = flag.String("key", "dev-key", "API key")
		scenario  = flag.String("scenario", "hot", "hot (all transfers on 2 accounts) | spread (transfers across -accounts accounts) | read (balance reads)")
		stage     = flag.Duration("stage", 10*time.Second, "duration of each load stage")
		ramp      = flag.Duration("ramp", time.Second, "time to linearly stagger worker start within each stage (capped to half -stage)")
		start     = flag.Int("start", 8, "workers in the first stage (doubles every stage)")
		maxW      = flag.Int("max", 512, "maximum workers")
		nAccounts = flag.Int("accounts", 100, "accounts used by the spread and read scenarios")
		errLimit  = flag.Float64("err-limit", 0.05, "failure ratio (checked separately for app and connection failures) that counts as broken")
		p99Limit  = flag.Duration("p99-limit", 2*time.Second, "p99 latency that counts as broken")
		seed      = flag.Int64("seed", 0, "PRNG seed for account/pair selection; 0 derives one from the current time and prints it")
	)
	flag.Parse()

	if err := run(*base, *key, *scenario, *stage, *ramp, *start, *maxW, *nAccounts, *errLimit, *p99Limit, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run(base, key, scenario string, stage, ramp time.Duration, start, maxW, nAccounts int, errLimit float64, p99Limit time.Duration, seed int64) error {
	if err := isLocal(base); err != nil {
		return err
	}
	if start < 1 || maxW < start {
		return errors.New("need 1 <= -start <= -max")
	}
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	fmt.Printf("Seed: %d (pass -seed %d to repeat the account/pair selection of this run)\n", seed, seed)

	n := nAccounts
	switch scenario {
	case "hot":
		n = 2
	case "spread", "read":
		if n < 2 {
			return errors.New("-accounts must be at least 2")
		}
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}

	c := newClient(base, key, maxW)
	// runID only namespaces account owners and idempotency keys so repeat
	// runs (including repeats with the same -seed, for reproducibility)
	// never collide with accounts a previous run already created.
	runID := time.Now().UnixNano()

	fmt.Printf("Setting up %d funded accounts (scenario %q)...\n", n, scenario)
	ids := make([]string, n)
	for i := range ids {
		id, err := c.createAccount(fmt.Sprintf("load-%d-%d", runID, i))
		if err != nil {
			return fmt.Errorf("setup (is the API running?): %w", err)
		}
		if err := c.deposit(id, fundingCents, fmt.Sprintf("fund-%d-%d", runID, i)); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		ids[i] = id
	}

	var seq atomic.Uint64
	transfer := func(from, to string) target {
		body := fmt.Sprintf(`{"from_account_id":%q,"to_account_id":%q,"amount_cents":1}`, from, to)
		return target{
			method:  http.MethodPost,
			path:    "/transfers",
			body:    []byte(body),
			idemKey: fmt.Sprintf("load-%d-%d", runID, seq.Add(1)),
		}
	}
	var next func(*rand.Rand) target
	switch scenario {
	case "hot":
		next = func(*rand.Rand) target { return transfer(ids[0], ids[1]) }
	case "spread":
		next = func(rng *rand.Rand) target {
			i := rng.IntN(n)
			j := (i + 1 + rng.IntN(n-1)) % n
			return transfer(ids[i], ids[j])
		}
	case "read":
		next = func(rng *rand.Rand) target {
			return target{method: http.MethodGet, path: "/accounts/" + ids[rng.IntN(n)] + "/balance"}
		}
	}

	fmt.Printf("Ramping up: %d workers, doubling every %s (staggered over %s) until app or connection failures exceed %.0f%% or p99 > %s (max %d workers)\n\n",
		start, stage, ramp, errLimit*100, p99Limit, maxW)

	report := benchReport{
		Timestamp: time.Now(),
		Scenario:  scenario,
		Seed:      seed,
		Params: benchParams{
			Base:          base,
			StageDuration: stage.String(),
			RampUp:        ramp.String(),
			StartWorkers:  start,
			MaxWorkers:    maxW,
			Accounts:      n,
			ErrLimit:      errLimit,
			P99Limit:      p99Limit.String(),
		},
	}

	brokeAt, why := 0, ""
	for w := start; w <= maxW; w *= 2 {
		res := runStage(c, next, w, stage, ramp, seed)
		res.report()
		v := evaluateStage(res, errLimit, p99Limit)
		report.Stages = append(report.Stages, toBenchStage(res, v))
		if v.broke {
			brokeAt, why = w, v.reason
			report.BrokeAt = w
			report.BrokeReason = why
			break
		}
	}

	fmt.Println()
	if brokeAt == 0 {
		fmt.Printf("The API did NOT break up to %d workers.\n", maxW)
	} else {
		fmt.Printf("BROKE at %d workers: %s\n", brokeAt, why)
	}

	fmt.Println("\nWaiting for the API to recover...")
	began := time.Now()
	for {
		if _, err := c.balance(ids[0]); err == nil {
			report.RecoveredIn = time.Since(began).Round(time.Millisecond).String()
			fmt.Printf("Recovered after %s.\n", report.RecoveredIn)
			break
		}
		if time.Since(began) > 60*time.Second {
			fmt.Println("Did NOT recover within 60s.")
			_, _ = writeBenchReport(report)
			return errors.New("service did not recover")
		}
		time.Sleep(500 * time.Millisecond)
	}

	var sum int64
	for _, id := range ids {
		b, err := c.balance(id)
		if err != nil {
			_, _ = writeBenchReport(report)
			return fmt.Errorf("consistency check: %w", err)
		}
		sum += b
	}
	want := fundingCents * int64(n)
	report.Consistency = &benchConsistency{Passed: sum == want, ActualCents: sum, ExpectedCents: want, DiffCents: sum - want}

	path, err := writeBenchReport(report)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest: warning: could not write bench report:", err)
	} else {
		fmt.Printf("Report written to %s\n", path)
	}

	if report.Consistency.Passed {
		fmt.Printf("Consistency check PASSED: balances sum to %d cents, no money created or lost.\n", sum)
	} else {
		fmt.Printf("Consistency check FAILED: balances sum to %d cents, expected %d (diff %d).\n", sum, want, sum-want)
		return errors.New("ledger inconsistent after load")
	}
	return nil
}
