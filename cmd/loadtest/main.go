// Command loadtest ramps up load against a locally running wallet-go API
// until it breaks, or until the host runs out of the CPU or RAM budget,
// then checks that the ledger is still consistent.
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
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

const fundingCents int64 = 1_000_000_000

type options struct {
	base, key, scenario   string
	stage, ramp, p99Limit time.Duration
	start, maxW, accounts int
	errLimit, rate        float64
	cpuLimit, ramLimit    float64
	apiMaxConns           int
	seed                  int64
}

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

// resourceGuard samples host CPU and RAM while load runs and trips when one
// of them exceeds its limit. It is host-wide on purpose: the generator, the
// API and PostgreSQL all compete for the same machine, so the budget is the
// machine's, not the process's. A limit of 0 disables that check.
type resourceGuard struct {
	cpuLimit float64
	ramLimit float64
	tripped  atomic.Bool
	mu       sync.Mutex
	reason   string
}

func (g *resourceGuard) trip(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reason == "" {
		g.reason = reason
	}
	g.tripped.Store(true)
}

func (g *resourceGuard) why() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}

// watch runs until ctx is cancelled or a limit trips. cpu.PercentWithContext
// blocks for its sampling interval, which doubles as the loop's pacing.
func (g *resourceGuard) watch(ctx context.Context) {
	for ctx.Err() == nil {
		if g.cpuLimit > 0 {
			pct, err := cpu.PercentWithContext(ctx, 500*time.Millisecond, false)
			if err == nil && len(pct) > 0 && pct[0] > g.cpuLimit {
				g.trip(fmt.Sprintf("host CPU %.1f%% above %.0f%%", pct[0], g.cpuLimit))
				return
			}
		} else {
			time.Sleep(500 * time.Millisecond)
		}
		if g.ramLimit > 0 {
			vm, err := mem.VirtualMemoryWithContext(ctx)
			if err == nil && vm.UsedPercent > g.ramLimit {
				g.trip(fmt.Sprintf("host RAM %.1f%% above %.0f%%", vm.UsedPercent, g.ramLimit))
				return
			}
		}
	}
}

// runStage drives workers concurrent request loops for duration d. Workers
// are not all started at once: their first request is staggered linearly
// over ramp (capped to half of d), so a stage does not open hundreds of TCP
// connections in the same instant. seed makes the sequence of accounts/pairs
// each worker picks deterministic. rate (total req/s, 0 = closed loop) paces
// each worker so the stage as a whole aims at that rate. The stage ends early
// if guard trips.
func runStage(c *client, next func(*rand.Rand) target, workers int, d, ramp time.Duration, seed int64, rate float64, guard *resourceGuard) stageResult {
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
			var interval time.Duration
			if rate > 0 {
				interval = time.Duration(float64(time.Second) * float64(workers) / rate)
			}

			var lat []time.Duration
			statuses := map[int]int{}
			errs := map[string]int{}
			for time.Now().Before(deadline) && !guard.tripped.Load() {
				t := next(rng)
				t0 := time.Now()
				status, err := c.send(t)
				lat = append(lat, time.Since(t0))
				if err != nil {
					errs[classify(err)]++
				} else {
					statuses[status]++
				}
				if wait := interval - time.Since(t0); interval > 0 && wait > 0 {
					time.Sleep(wait)
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

// verdict is the break/no-break call for one stage. resource marks a stop
// caused by the host budget, which says nothing about the API's own limits.
type verdict struct {
	broke    bool
	resource bool
	reason   string
}

func evaluateStage(r stageResult, errLimit float64, p99Limit time.Duration) verdict {
	if p99 := r.percentile(0.99); p99 > p99Limit {
		return verdict{broke: true, reason: fmt.Sprintf("p99 latency %s is above %s", p99.Round(time.Millisecond), p99Limit)}
	}
	if af := r.appFailureRatio(); af > errLimit {
		return verdict{broke: true, reason: fmt.Sprintf("application failure ratio %.1f%% (non-2xx responses) is above %.1f%%", af*100, errLimit*100)}
	}
	if cf := r.connFailureRatio(); cf > errLimit {
		return verdict{broke: true, reason: fmt.Sprintf("connection failure ratio %.1f%% (refused/reset/timeout) is above %.1f%% -- this can be a client/OS limit rather than API saturation, see docs/loadtest.md", cf*100, errLimit*100)}
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

// --- bench report (docs/bench/<scenario>-seed<seed>-<timestamp>.json) ---

type benchReport struct {
	Timestamp   time.Time         `json:"timestamp"`
	Scenario    string            `json:"scenario"`
	Seed        int64             `json:"seed"`
	Environment benchEnvironment  `json:"environment"`
	Baseline    benchBaseline     `json:"baseline"`
	Params      benchParams       `json:"params"`
	Stages      []benchStage      `json:"stages"`
	StopKind    string            `json:"stop_kind"`
	StopReason  string            `json:"stop_reason,omitempty"`
	RecoveredIn string            `json:"recovered_in,omitempty"`
	Consistency *benchConsistency `json:"consistency,omitempty"`
}

type benchEnvironment struct {
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	NumCPU          int    `json:"num_cpu"`
	GoVersion       string `json:"go_version"`
	PostgresVersion string `json:"postgres_version"`
	DBSizeBytes     int64  `json:"db_size_bytes"`
}

// benchBaseline is the host's load measured just before the ramp starts, with
// nothing from this run on it yet.
type benchBaseline struct {
	CPUPercent float64 `json:"cpu_percent"`
	RAMPercent float64 `json:"ram_percent"`
}

type benchParams struct {
	// APIMaxConns is what the operator declared the API was started with. The
	// tool cannot read it from the API process, so it is a declaration, not a
	// measurement; 0 means it was not declared.
	APIMaxConns   int     `json:"api_max_conns_declared"`
	Base          string  `json:"base"`
	StageDuration string  `json:"stage_duration"`
	RampUp        string  `json:"ramp_up"`
	StartWorkers  int     `json:"start_workers"`
	MaxWorkers    int     `json:"max_workers"`
	Accounts      int     `json:"accounts"`
	Rate          float64 `json:"rate"`
	ErrLimit      float64 `json:"err_limit"`
	P99Limit      string  `json:"p99_limit"`
	CPULimit      float64 `json:"cpu_limit"`
	RAMLimit      float64 `json:"ram_limit"`
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

// captureEnvironment records where and on what the numbers were produced.
// It deliberately skips the hostname and the DSN: docs/bench/ is committed to
// a public repository, and the DSN contains a password.
func captureEnvironment(ctx context.Context, dsn string) (benchEnvironment, error) {
	env := benchEnvironment{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
		GoVersion: runtime.Version(),
	}
	if dsn == "" {
		return env, errors.New("DATABASE_URL not set, postgres fields left empty")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return env, err
	}
	defer func() { _ = conn.Close(ctx) }()
	err = conn.QueryRow(ctx, `SELECT version(), pg_database_size(current_database())`).
		Scan(&env.PostgresVersion, &env.DBSizeBytes)
	return env, err
}

// writeBenchReport saves one JSON file per execution under docs/bench/. The
// timestamp in the name keeps repeated runs with the same seed from
// overwriting each other.
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
	_ = godotenv.Load()

	var o options
	flag.StringVar(&o.base, "base", "http://localhost:8080", "API base URL (localhost only)")
	flag.StringVar(&o.key, "key", "dev-key", "API key")
	flag.StringVar(&o.scenario, "scenario", "hot", "hot (all transfers on 2 accounts) | spread (transfers across -accounts accounts) | read (balance reads)")
	flag.DurationVar(&o.stage, "stage", 10*time.Second, "duration of each load stage")
	flag.DurationVar(&o.ramp, "ramp", time.Second, "time to linearly stagger worker start within each stage (capped to half -stage)")
	flag.IntVar(&o.start, "start", 8, "workers in the first stage (doubles every stage)")
	flag.IntVar(&o.maxW, "max", 512, "maximum workers")
	flag.IntVar(&o.accounts, "accounts", 100, "accounts used by the spread and read scenarios")
	flag.Float64Var(&o.errLimit, "err-limit", 0.05, "failure ratio (checked separately for app and connection failures) that counts as broken")
	flag.DurationVar(&o.p99Limit, "p99-limit", 2*time.Second, "p99 latency that counts as broken")
	flag.Float64Var(&o.rate, "rate", 0, "target total requests/s across all workers; 0 = closed loop, as fast as possible")
	flag.Float64Var(&o.cpuLimit, "cpu-limit", 100, "stop when host CPU usage (%) exceeds this; 0 disables")
	flag.Float64Var(&o.ramLimit, "ram-limit", 90, "stop when host RAM usage (%) exceeds this; 0 disables")
	flag.IntVar(&o.apiMaxConns, "api-max-conns", 0, "MaxConns the API was started with, recorded as declared in the report (0 = not declared)")
	flag.Int64Var(&o.seed, "seed", 0, "PRNG seed for account/pair selection; 0 derives one from the current time and prints it")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if err := isLocal(o.base); err != nil {
		return err
	}
	if o.start < 1 || o.maxW < o.start {
		return errors.New("need 1 <= -start <= -max")
	}
	if o.seed == 0 {
		o.seed = time.Now().UnixNano()
	}
	fmt.Printf("Seed: %d (pass -seed %d to repeat the account/pair selection of this run)\n", o.seed, o.seed)

	n := o.accounts
	switch o.scenario {
	case "hot":
		n = 2
	case "spread", "read":
		if n < 2 {
			return errors.New("-accounts must be at least 2")
		}
	default:
		return fmt.Errorf("unknown scenario %q", o.scenario)
	}

	env, envErr := captureEnvironment(context.Background(), os.Getenv("DATABASE_URL"))
	if envErr != nil {
		fmt.Fprintln(os.Stderr, "loadtest: warning: environment:", envErr)
	}

	c := newClient(o.base, o.key, o.maxW)
	// runID only namespaces account owners and idempotency keys so repeat
	// runs (including repeats with the same -seed) never collide with
	// accounts a previous run already created.
	runID := time.Now().UnixNano()

	fmt.Printf("Setting up %d funded accounts (scenario %q)...\n", n, o.scenario)
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
	switch o.scenario {
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

	guard := &resourceGuard{cpuLimit: o.cpuLimit, ramLimit: o.ramLimit}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	var baseline benchBaseline
	if pct, err := cpu.PercentWithContext(watchCtx, 500*time.Millisecond, false); err == nil && len(pct) > 0 {
		baseline.CPUPercent = pct[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		baseline.RAMPercent = vm.UsedPercent
	}
	fmt.Printf("Baseline host usage: CPU %.1f%%, RAM %.1f%% (limits: CPU %.0f%%, RAM %.0f%%, 0 = disabled)\n",
		baseline.CPUPercent, baseline.RAMPercent, o.cpuLimit, o.ramLimit)
	if o.cpuLimit > 0 || o.ramLimit > 0 {
		go guard.watch(watchCtx)
	}

	fmt.Printf("Ramping up: %d workers, doubling every %s (staggered over %s) until app or connection failures exceed %.0f%%, p99 > %s, or host CPU/RAM pass their limits (max %d workers)\n\n",
		o.start, o.stage, o.ramp, o.errLimit*100, o.p99Limit, o.maxW)

	report := benchReport{
		Timestamp:   time.Now(),
		Scenario:    o.scenario,
		Seed:        o.seed,
		Environment: env,
		Baseline:    baseline,
		Params: benchParams{
			APIMaxConns:   o.apiMaxConns,
			Base:          o.base,
			StageDuration: o.stage.String(),
			RampUp:        o.ramp.String(),
			StartWorkers:  o.start,
			MaxWorkers:    o.maxW,
			Accounts:      n,
			Rate:          o.rate,
			ErrLimit:      o.errLimit,
			P99Limit:      o.p99Limit.String(),
			CPULimit:      o.cpuLimit,
			RAMLimit:      o.ramLimit,
		},
		StopKind: "max_workers",
	}

	for w := o.start; w <= o.maxW; w *= 2 {
		res := runStage(c, next, w, o.stage, o.ramp, o.seed, o.rate, guard)
		res.report()
		v := evaluateStage(res, o.errLimit, o.p99Limit)
		if !v.broke && guard.tripped.Load() {
			v = verdict{broke: true, resource: true, reason: "host resource limit: " + guard.why()}
		}
		report.Stages = append(report.Stages, toBenchStage(res, v))
		if v.broke {
			report.StopReason = v.reason
			report.StopKind = "api_broke"
			if v.resource {
				report.StopKind = "resource_limit"
			}
			fmt.Println()
			if v.resource {
				fmt.Printf("STOPPED at %d workers by the host budget: %s\n", w, v.reason)
			} else {
				fmt.Printf("BROKE at %d workers: %s\n", w, v.reason)
			}
			break
		}
	}
	stopWatch()

	if report.StopKind == "max_workers" {
		fmt.Printf("\nThe API did NOT break up to %d workers.\n", o.maxW)
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
