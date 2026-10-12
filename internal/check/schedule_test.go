package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	modelA = "magpie-252-ca704414/codex/gpt-6-astra"
	modelB = "magpie-252-8b686a70/codex/gpt-6-astra"
)

// A fixed, movable clock: the scheduler takes `now` as input and the classification
// of a finished batch reads the service clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var traceBase = time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)

// goldenTexts returns the three synthetic answers of a batch the reference
// JavaScript scorer classified as `prediction`.
func goldenTexts(t *testing.T, prediction string) [3]string {
	t.Helper()
	for _, c := range loadGolden(t) {
		if c.JS.Prediction == prediction && len(c.Outputs) == 3 {
			return [3]string{c.Outputs[0].Text, c.Outputs[1].Text, c.Outputs[2].Text}
		}
	}
	t.Fatal("no synthetic batch for", prediction)
	return [3]string{}
}

// traceUpstream is a local fixture gateway; no probe ever leaves the machine.
type traceUpstream struct {
	calls atomic.Int32
	mu    sync.Mutex
	mode  string
	texts map[string][3]string
	hold  chan struct{}
	url   string
}

func newUpstream(t *testing.T) *traceUpstream {
	t.Helper()
	up := &traceUpstream{mode: "astra", texts: map[string][3]string{"astra": goldenTexts(t, "gpt-6-astra"), "sol": goldenTexts(t, "gpt-5.6-sol")}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(up.calls.Add(1))
		up.mu.Lock()
		mode, hold := up.mode, up.hold
		up.mu.Unlock()
		if hold != nil {
			<-hold
		}
		switch mode {
		case "astra", "sol":
			body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": up.texts[mode][(n-1)%3]}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 12}})
			w.Write(body)
		case "short":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"1,2,3"},"finish_reason":"stop"}],"usage":{"total_tokens":12}}`)
		case "drop":
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		default:
			var status int
			fmt.Sscan(mode, &status)
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(srv.Close)
	up.url = srv.URL
	return up
}

func (u *traceUpstream) set(mode string) { u.mu.Lock(); u.mode = mode; u.mu.Unlock() }

func scheduledService(t *testing.T, dir, url string, clk *fakeClock, models ...string) *Service {
	t.Helper()
	s, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if clk != nil {
		s.clock = clk.Now
	}
	if err := s.Configure(Config{GatewayURL: url, GatewayKey: "fixture", DailyLimit: 120}); err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if err := s.SetTarget(m, true); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func plan(models ...string) map[string]TraceModelPlan {
	out := map[string]TraceModelPlan{}
	for _, m := range models {
		out[m] = TraceModelPlan{IntervalMinutes: defaultTraceInterval, DailyCap: 12}
	}
	return out
}

func waitTraceIdle(t *testing.T, s *Service, results int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _ := s.Snapshot()
		idle := len(state.Results) == results
		for _, r := range state.Results {
			idle = idle && r.Verdict != "running"
		}
		if idle {
			// The batch is classified in the save of its last result; wait for the goroutine to leave.
			s.mu.Lock()
			busy := len(s.running) > 0
			s.mu.Unlock()
			if !busy {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d finished results, have %d", results, len(state.Results))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runOutcome(s *Service, model string) TraceRun {
	state, _ := s.Snapshot()
	if run := state.TraceSchedule.Runs[model]; run != nil {
		return *run
	}
	return TraceRun{}
}

// tickAt moves the clock to `at`, runs one scheduler tick and waits for the batch it started.
func tickAt(t *testing.T, s *Service, clk *fakeClock, at time.Time, wantResults int) {
	t.Helper()
	clk.Set(at)
	s.scheduleTick(at)
	waitTraceIdle(t, s, wantResults)
}

func setSched(s *Service, at time.Time, models map[string]TraceModelPlan) error {
	return s.setTraceScheduleAt(true, models, at)
}

func TestTraceScheduleDisabledByDefault(t *testing.T) {
	up := newUpstream(t)
	s := scheduledService(t, t.TempDir(), up.url, nil, modelA)
	state, _ := s.Snapshot()
	if state.TraceSchedule.Enabled || len(state.TraceSchedule.Models) != 0 {
		t.Fatal("schedule must default to off with an empty allowlist")
	}
	if wait := s.scheduleTick(traceBase); wait != 0 || up.calls.Load() != 0 || len(s.TraceScheduleStatus(traceBase)) != 0 {
		t.Fatal("disabled schedule acted")
	}
	if err := s.setTraceScheduleAt(true, nil, traceBase); err != nil {
		t.Fatal(err)
	}
	if wait := s.scheduleTick(traceBase.Add(time.Hour)); wait != 0 || up.calls.Load() != 0 {
		t.Fatal("enabled schedule with empty allowlist acted")
	}
	if err := s.setTraceScheduleAt(false, plan(modelA), traceBase); err != nil {
		t.Fatal(err)
	}
	if wait := s.scheduleTick(traceBase.Add(24 * time.Hour)); wait != 0 || up.calls.Load() != 0 {
		t.Fatal("disabled schedule with allowlist acted")
	}
}

func TestTraceScheduleLoadsStateWithoutScheduleField(t *testing.T) {
	dir := t.TempDir()
	old, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.store.db.Exec(`UPDATE state SET body=?`, `{"config":{"gateway_url":"http://127.0.0.1:1","gateway_key":"k","daily_limit":77},"targets":{"`+modelA+`":{"model":"`+modelA+`","enabled":true,"state":"passed"}},"results":[],"alias":"abc"}`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, _ := s.Snapshot()
	if state.TraceSchedule.Enabled || state.TraceSchedule.Models == nil || state.Config.DailyLimit != 77 || state.Targets[modelA].State != "passed" {
		t.Fatalf("existing config not preserved: %+v", state)
	}
	if err := s.Configure(Config{GatewayURL: "http://127.0.0.1:2", DailyLimit: 50}); err != nil {
		t.Fatal(err)
	}
	if err := setSched(s, traceBase, plan(modelA)); err != nil {
		t.Fatal(err)
	}
	if err := s.Configure(Config{GatewayURL: "http://127.0.0.1:3", DailyLimit: 60}); err != nil {
		t.Fatal(err)
	}
	if state, _ = s.Snapshot(); !state.TraceSchedule.Enabled || len(state.TraceSchedule.Models) != 1 {
		t.Fatal("gateway configuration wiped the schedule")
	}
}

func TestTraceScheduleValidation(t *testing.T) {
	s := scheduledService(t, t.TempDir(), "http://127.0.0.1:1", nil, modelA, "a/solo")
	for name, models := range map[string]map[string]TraceModelPlan{
		"unknown model":      {"a/ghost": {DailyCap: 1}},
		"fixed baseline":     {modelA: {IntervalMinutes: 60, DailyCap: 1}},
		"short":              {modelA: {IntervalMinutes: 5, DailyCap: 1}},
		"cap":                {modelA: {DailyCap: maxTraceDailyCap + 1}},
		"negative cap":       {modelA: {DailyCap: -1}},
		"no bank candidate":  {"a/solo": {DailyCap: 1}},
		"expect not in bank": {modelA: {DailyCap: 1, Expect: "gpt-9-imaginary"}},
		"expect on non-bank": {"a/solo": {DailyCap: 1, Expect: "nope"}},
	} {
		if err := setSched(s, traceBase, models); err == nil {
			t.Fatal("accepted", name)
		}
	}
	if err := setSched(s, traceBase, map[string]TraceModelPlan{modelA: {}, "a/solo": {Expect: "gpt-5.5"}}); err != nil {
		t.Fatal(err)
	}
	state, _ := s.Snapshot()
	if p := state.TraceSchedule.Models[modelA]; p.IntervalMinutes != defaultTraceInterval || p.DailyCap != defaultTraceDailyCap {
		t.Fatal("conservative defaults not applied", p)
	}
}

func TestTraceCadenceLadderAndStaleThreshold(t *testing.T) {
	for streak, want := range map[int]time.Duration{-1: 15 * time.Minute, 0: 15 * time.Minute, 1: 30 * time.Minute, 2: time.Hour, 3: 2 * time.Hour, 4: 4 * time.Hour, 5: 4 * time.Hour, 34: 4 * time.Hour} {
		if got := traceCadence(streak); got != want {
			t.Fatalf("streak %d: %v, want %v", streak, got, want)
		}
	}
	last := traceBase
	run := &TraceRun{Attempt: last, Streak: 8, LastMatchAt: last, Class: classMatch}
	if traceStreakAt(run, last.Add(traceStreakTTL)) != 8 {
		t.Fatal("a streak exactly at the stale threshold must still count")
	}
	if traceStreakAt(run, last.Add(traceStreakTTL+time.Second)) != 0 {
		t.Fatal("a streak past the stale threshold must void")
	}
	if got := traceDueAt(run, last.Add(time.Hour)); !got.Equal(last.Add(4 * time.Hour)) {
		t.Fatal("fresh streak 8 is not on the 4h step", got)
	}
	if got := traceDueAt(run, last.Add(7*time.Hour)); !got.Equal(last.Add(15 * time.Minute)) {
		t.Fatal("stale streak did not fall back to 15 minutes", got)
	}
	if !traceDueAt(&TraceRun{}, last).IsZero() || !traceDueAt(nil, last).IsZero() {
		t.Fatal("an unanchored run must report no due time")
	}
}

func TestTraceClassifyBatch(t *testing.T) {
	astra, sol := goldenTexts(t, "gpt-6-astra"), goldenTexts(t, "gpt-5.6-sol")
	batch := func(texts [3]string, mutate func(i int, r *Result)) []Result {
		out := make([]Result, 3)
		for i := range out {
			out[i] = Result{Verdict: "manual", Answer: texts[i], ExpectedCount: 300}
			if mutate != nil {
				mutate(i, &out[i])
			}
		}
		return out
	}
	fail := func(i int, status int, kind string) func(int, *Result) {
		return func(j int, r *Result) {
			if j == i {
				r.Verdict, r.Error, r.Answer, r.HTTPStatus, r.ErrorKind = "unavailable", "boom", "", status, kind
			}
		}
	}
	for name, tc := range map[string]struct {
		results []Result
		expect  string
		want    string
	}{
		"full astra batch matches":   {batch(astra, nil), "gpt-6-astra", classMatch},
		"full sol batch mismatches": {batch(sol, nil), "gpt-6-astra", classMismatch},
		"no expected candidate":      {batch(astra, nil), "", classUnknown},
		"two results only":           {batch(astra, nil)[:2], "gpt-6-astra", classUnknown},
		"529 on the first probe":     {batch(astra, fail(0, 529, "")), "gpt-6-astra", class529},
		"529 beats transient":        {batch(astra, func(i int, r *Result) { fail(0, 0, "timeout")(i, r); fail(2, 529, "")(i, r) }), "gpt-6-astra", class529},
		"timeout":                    {batch(astra, fail(1, 0, "timeout")), "gpt-6-astra", classTransient},
		"network":                    {batch(astra, fail(2, 0, "network")), "gpt-6-astra", classTransient},
		"503":                        {batch(astra, fail(0, 503, "")), "gpt-6-astra", classTransient},
		"400 is not transient":       {batch(astra, fail(0, 400, "")), "gpt-6-astra", classUnknown},
		"untyped error":              {batch(astra, fail(0, 0, "")), "gpt-6-astra", classUnknown},
		"cut-off answer":             {batch(astra, func(i int, r *Result) { r.Error = "回复未正常结束或被截断" }), "gpt-6-astra", classUnknown},
		"bare answer":                {batch([3]string{"1,2,3", "4,5,6", "7,8,9"}, nil), "gpt-6-astra", classUnknown},
		"one short answer of three": {batch(astra, func(i int, r *Result) {
			if i == 2 {
				r.Answer = "1, 2, 3"
			}
		}), "gpt-6-astra", classUnknown},
		"missing requested count": {batch(astra, func(i int, r *Result) { r.ExpectedCount = 0 }), "gpt-6-astra", classUnknown},
	} {
		got, report := classifyTraceBatch(tc.results, tc.expect)
		if got != tc.want {
			t.Fatalf("%s: %s, want %s", name, got, tc.want)
		}
		if (report != nil) != (got == classMatch || got == classMismatch) {
			t.Fatalf("%s: report presence wrong", name)
		}
	}
}

func TestTraceFailureTypedFacts(t *testing.T) {
	timeout := &net.OpError{Op: "dial", Err: timeoutErr{}}
	for name, tc := range map[string]struct {
		err    error
		status int
		kind   string
	}{
		"http":     {&HTTPStatusError{Status: 529}, 529, ""},
		"wrapped":  {fmt.Errorf("x: %w", &HTTPStatusError{Status: 503}), 503, ""},
		"deadline": {context.DeadlineExceeded, 0, "timeout"},
		"timeout":  {timeout, 0, "timeout"},
		"plain":    {fmt.Errorf("post: %w", errors.New("x")), 0, ""},
		"eof":      {fmt.Errorf("post: %w", io.EOF), 0, "network"},
		"canceled": {context.Canceled, 0, ""},
		"opError":  {&net.OpError{Op: "read", Err: errors.New("reset")}, 0, "network"},
		"text":     {errors.New("Magpie HTTP 529"), 0, ""},
	} {
		if status, kind := traceFailure(tc.err); status != tc.status || kind != tc.kind {
			t.Fatalf("%s: %d %q", name, status, kind)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The core acceptance: a full three-answer batch whose closest candidate is gpt-6-astra
// advances the streak, and the next run follows the old 15/30/60/120/240 minute steps.
func TestTraceScheduleStreakLadder(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	if err := setSched(s, traceBase, plan(modelA)); err != nil {
		t.Fatal(err)
	}
	if s.scheduleTick(traceBase) <= 0 || up.calls.Load() != 0 {
		t.Fatal("a freshly enabled model must wait one baseline")
	}
	at := traceBase
	for i, gap := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		clk.Set(at.Add(gap - time.Second))
		s.scheduleTick(clk.Now())
		if int(up.calls.Load()) != 3*i {
			t.Fatalf("step %d started %v early", i, time.Second)
		}
		at = at.Add(gap)
		tickAt(t, s, clk, at, 3*(i+1))
		run := runOutcome(s, modelA)
		if run.Class != classMatch || run.Streak != i+1 || !run.Attempt.Equal(at) || !run.LastMatchAt.Equal(at) ||
			run.Fingerprint == nil || run.Fingerprint.Prediction != "gpt-6-astra" || run.Fingerprint.Expect != "gpt-6-astra" {
			t.Fatalf("step %d: %+v", i, run)
		}
		wantNext := traceCadence(i + 1)
		if got := s.TraceScheduleStatus(at)[0]; !got.NextAt.Equal(at.Add(wantNext)) || got.CadenceMinutes != int(wantNext/time.Minute) || got.Streak != i+1 || !strings.Contains(got.Note, "不能证明") {
			t.Fatalf("step %d status: %+v", i, got)
		}
	}
	if up.calls.Load() != 21 {
		t.Fatal("unexpected request count", up.calls.Load())
	}
}

func TestTraceScheduleMismatchAndUnknownResetStreak(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	s.mu.Lock()
	s.state.TraceSchedule.Runs[modelA] = &TraceRun{Attempt: traceBase, Streak: 8, LastMatchAt: traceBase, Class: classMatch}
	s.mu.Unlock()

	up.set("sol")
	at := traceBase.Add(4 * time.Hour)
	tickAt(t, s, clk, at, 3)
	run := runOutcome(s, modelA)
	if run.Class != classMismatch || run.Streak != 0 || run.Fingerprint.Prediction != "gpt-5.6-sol" {
		t.Fatalf("mismatch not recorded: %+v", run)
	}
	if got := s.TraceScheduleStatus(at)[0]; !got.NextAt.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("mismatch should be retried after the baseline: %+v", got)
	}

	up.set("short")
	at = at.Add(15 * time.Minute)
	tickAt(t, s, clk, at, 6)
	run = runOutcome(s, modelA)
	if run.Class != classUnknown || run.Streak != 0 || !run.Paused {
		t.Fatalf("unknown result: %+v", run)
	}

	up.set("astra")
	at = at.Add(15 * time.Minute)
	s.scheduleTick(at)
	if up.calls.Load() != 6 {
		t.Fatal("unknown result must pause the scheduler")
	}
	clk.Set(at)
	if err := s.TriggerModelTrace(modelA); err != nil {
		t.Fatal(err)
	}
	waitTraceIdle(t, s, 9)
	if run = runOutcome(s, modelA); run.Paused || run.Streak != 1 || !traceDueAt(&run, at).Equal(at.Add(30*time.Minute)) {
		t.Fatalf("match after reset should be streak 1 on the 30 minute step: %+v", run)
	}
}

func TestTraceScheduleDegraded529Ladder(t *testing.T) {
	up := newUpstream(t)
	up.set("529")
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, s, clk, at, 3)
	for i, wait := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		run := runOutcome(s, modelA)
		if run.Class != class529 || run.Streak529 != i+1 || run.Streak != 0 || !run.At529.Equal(at) || !traceDueAt(&run, at).Equal(at.Add(wait)) {
			t.Fatalf("529 #%d: %+v", i+1, run)
		}
		if int(up.calls.Load()) != i+1 {
			t.Fatalf("a 529 batch must stop after the first probe, %d calls after %d batches", up.calls.Load(), i+1)
		}
		clk.Set(at.Add(wait - time.Second))
		s.scheduleTick(clk.Now())
		if int(up.calls.Load()) != i+1 {
			t.Fatal("retried before the 529 wait ended")
		}
		at = at.Add(wait)
		if i < 4 {
			tickAt(t, s, clk, at, 3*(i+2))
		}
	}
	state, _ := s.Snapshot()
	for _, r := range state.Results[1:3] {
		if r.Verdict != "unavailable" || r.Error == "" || r.HTTPStatus != 0 {
			t.Fatalf("unsent probe not recorded as not sent: %+v", r)
		}
	}
	// Only a scored batch ends the marker; a bare 200 does not.
	up.set("short")
	tickAt(t, s, clk, at, 18)
	run := runOutcome(s, modelA)
	if run.Class != classUnknown || run.Streak529 != 5 || !run.Paused {
		t.Fatalf("unknown must not clear the 529 marker: %+v", run)
	}
	up.set("astra")
	at = at.Add(15 * time.Minute)
	calls := up.calls.Load()
	s.scheduleTick(at.Add(24 * time.Hour))
	if up.calls.Load() != calls {
		t.Fatal("unknown result must pause the scheduler")
	}
	clk.Set(at)
	if err := s.TriggerModelTrace(modelA); err != nil {
		t.Fatal(err)
	}
	waitTraceIdle(t, s, 21)
	if run = runOutcome(s, modelA); run.Streak529 != 0 || !run.At529.IsZero() || !run.NextDue.IsZero() || run.Streak != 1 {
		t.Fatalf("a scored batch must end the 529 marker: %+v", run)
	}
}

func TestTraceScheduleTransientRetryLadder(t *testing.T) {
	up := newUpstream(t)
	up.set("drop")
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, s, clk, at, 3)
	for i, wait := range []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute, 12 * time.Minute} {
		run := runOutcome(s, modelA)
		if run.Class != classTransient || run.Transient != i+1 || !traceDueAt(&run, at).Equal(at.Add(wait)) {
			t.Fatalf("transient #%d: %+v", i+1, run)
		}
		clk.Set(at.Add(wait - time.Second))
		s.scheduleTick(clk.Now())
		if int(up.calls.Load()) != i+1 {
			t.Fatal("retried before the cooldown ended")
		}
		at = at.Add(wait)
		tickAt(t, s, clk, at, 3*(i+2))
	}
	up.set("astra")
	tickAt(t, s, clk, at.Add(12*time.Minute), 18)
	if run := runOutcome(s, modelA); run.Transient != 0 || run.Streak != 1 || run.Class != classMatch {
		t.Fatalf("recovery: %+v", run)
	}
}

func TestTraceScheduleServerErrorIsTransient(t *testing.T) {
	up := newUpstream(t)
	up.set("503")
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	tickAt(t, s, clk, traceBase.Add(15*time.Minute), 3)
	if run := runOutcome(s, modelA); run.Class != classTransient || up.calls.Load() != 1 {
		t.Fatalf("5xx: %+v calls %d", run, up.calls.Load())
	}
}

func TestTraceScheduleOnceAcrossRestart(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	dir := t.TempDir()
	first := scheduledService(t, dir, up.url, clk, modelA)
	setSched(first, traceBase, plan(modelA))
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, first, clk, at, 3)
	first.scheduleTick(at.Add(time.Minute))
	if up.calls.Load() != 3 {
		t.Fatal("same attempt ran twice")
	}
	first.Close()

	second, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.clock = clk.Now
	second.scheduleTick(at.Add(time.Minute))
	second.scheduleTick(at.Add(29 * time.Minute))
	if state, _ := second.Snapshot(); up.calls.Load() != 3 || len(state.Results) != 3 {
		t.Fatal("restart repeated an attempt")
	}
	if run := runOutcome(second, modelA); run.Streak != 1 || run.Class != classMatch {
		t.Fatal("restart lost the streak", run)
	}
	tickAt(t, second, clk, at.Add(30*time.Minute), 6)
	if up.calls.Load() != 6 || runOutcome(second, modelA).Streak != 2 {
		t.Fatal("next step did not run", up.calls.Load())
	}
	if status := second.TraceScheduleStatus(at)[0]; status.RunState != "completed" || status.TodayRuns != 2 {
		t.Fatalf("bad status %+v", status)
	}
}

func TestTraceScheduleCrashAfterReservationDoesNotRetry(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	dir := t.TempDir()
	s := scheduledService(t, dir, up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	start := traceBase.Add(4 * time.Hour)
	s.mu.Lock()
	s.state.TraceSchedule.Runs[modelA] = &TraceRun{Period: start, Attempt: start, Outcome: "reserved", At: start, Day: "2026-10-12", DayRuns: 1, Streak: 8, LastMatchAt: start.Add(-4 * time.Hour), Class: classMatch}
	s.state.Results = append(s.state.Results, Result{ID: "r1", Model: modelA, Prompt: "modeltrace", Verdict: "running", Time: start})
	err := s.store.Save(s.state)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	restarted, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clock = clk.Now
	clk.Set(start.Add(time.Minute))
	restarted.scheduleTick(clk.Now())
	state, _ := restarted.Snapshot()
	if up.calls.Load() != 0 || len(state.Results) != 1 || state.Results[0].Verdict != "unavailable" {
		t.Fatal("crashed attempt was retried or running result kept")
	}
	if run := runOutcome(restarted, modelA); run.Outcome != "interrupted" || run.Streak != 8 {
		t.Fatal("interrupted attempt not reported or streak lost", run.Outcome)
	}
}

func TestTraceScheduleDownTimeRunsOnceWithoutBackfill(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	at := traceBase.Add(3 * 24 * time.Hour)
	tickAt(t, s, clk, at, 3)
	s.scheduleTick(at.Add(time.Second))
	if up.calls.Load() != 3 {
		t.Fatal("a long outage must produce one run, not a backlog", up.calls.Load())
	}
}

func TestTraceScheduleStaleStreakFallsBackToBaseline(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	attempt := traceBase.Add(-5 * time.Hour)
	s.mu.Lock()
	s.state.TraceSchedule.Runs[modelA] = &TraceRun{Attempt: attempt, Streak: 8, LastMatchAt: attempt, Class: classMatch}
	s.mu.Unlock()
	// Attempt+4h has passed too, but make it clear the stale rule is what is exercised: 5h old, still fresh.
	tickAt(t, s, clk, traceBase, 3)
	if run := runOutcome(s, modelA); run.Streak != 9 {
		t.Fatal("fresh streak should continue", run.Streak)
	}
	// Now 7h later: stale (>6h since the newest match) → due one baseline after the attempt, not 4h.
	run := runOutcome(s, modelA)
	if got := s.TraceScheduleStatus(run.LastMatchAt.Add(7 * time.Hour))[0]; got.CadenceMinutes != 15 {
		t.Fatalf("stale cadence %d", got.CadenceMinutes)
	}
}

func TestTraceScheduleSkipsBackOffAndRecover(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA, modelB, "magpie-252-00000000/codex/gpt-6-astra")
	setSched(s, traceBase, plan(modelA, modelB, "magpie-252-00000000/codex/gpt-6-astra"))
	s.SetTarget(modelB, false)
	if err := s.DeleteTarget("magpie-252-00000000/codex/gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	due := traceBase.Add(15 * time.Minute)
	clk.Set(due)
	if wait := s.scheduleTick(due); wait < time.Second {
		t.Fatal("a skip must not spin the loop", wait)
	}
	waitTraceIdle(t, s, 3)
	state, _ := s.Snapshot()
	for _, r := range state.Results {
		if r.Model != modelA {
			t.Fatal("disabled or deleted model probed", r.Model)
		}
	}
	for model, want := range map[string]string{modelA: "started", modelB: "target_disabled", "magpie-252-00000000/codex/gpt-6-astra": "target_missing"} {
		if got := runOutcome(s, model).Outcome; got != want {
			t.Fatalf("%s: %q, want %q", model, got, want)
		}
	}
	calls := up.calls.Load()
	s.scheduleTick(due.Add(time.Minute))
	if up.calls.Load() != calls || runOutcome(s, modelB).Retry.Before(due.Add(scheduleSkipRetry)) {
		t.Fatal("skipped model retried inside its back-off")
	}
	// The target comes back: it runs when the back-off ends, not before.
	s.SetTarget(modelB, true)
	s.scheduleTick(due.Add(4 * time.Minute))
	if up.calls.Load() != calls {
		t.Fatal("ran inside the back-off")
	}
	tickAt(t, s, clk, due.Add(scheduleSkipRetry), 6)
	if runOutcome(s, modelB).Outcome != "started" {
		t.Fatal("did not recover after the back-off", runOutcome(s, modelB).Outcome)
	}
}

func TestTraceScheduleDailyCap(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, map[string]TraceModelPlan{modelA: {DailyCap: 1}})
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, s, clk, at, 3)
	at = at.Add(30 * time.Minute)
	tickAt(t, s, clk, at, 3)
	run := runOutcome(s, modelA)
	if up.calls.Load() != 3 || run.Outcome != "daily_cap" || run.DayRuns != 1 {
		t.Fatal("daily cap not enforced", run.Outcome)
	}
	if !run.Retry.After(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("cap back-off should reach the next UTC day", run.Retry)
	}
	tickAt(t, s, clk, run.Retry, 6)
	if run = runOutcome(s, modelA); run.Outcome != "started" || run.DayRuns != 1 || run.Day != "2026-10-13" {
		t.Fatalf("did not resume on the next day: %+v", run)
	}
}

func TestTraceScheduleBudget(t *testing.T) {
	for _, tc := range []struct {
		used    int
		outcome string
		calls   int32
	}{{57, "started", 3}, {58, "budget", 0}} {
		t.Run(fmt.Sprint(tc.used), func(t *testing.T) {
			up := newUpstream(t)
			clk := &fakeClock{t: traceBase}
			s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
			setSched(s, traceBase, plan(modelA))
			at := traceBase.Add(15 * time.Minute)
			s.mu.Lock()
			for i := 0; i < tc.used; i++ {
				s.state.Results = append(s.state.Results, Result{ID: fmt.Sprint("seed", i), Model: modelA, Prompt: "milk", Verdict: "passed", Time: at})
			}
			s.mu.Unlock()
			clk.Set(at)
			s.scheduleTick(at)
			if tc.calls > 0 {
				waitTraceIdle(t, s, tc.used+3)
			}
			if run := runOutcome(s, modelA); run.Outcome != tc.outcome || up.calls.Load() != tc.calls {
				t.Fatalf("outcome %q calls %d", run.Outcome, up.calls.Load())
			}
		})
	}
}

func TestTraceScheduleSkipsRunningModel(t *testing.T) {
	up := newUpstream(t)
	up.hold = make(chan struct{})
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	setSched(s, traceBase, plan(modelA))
	if err := s.TriggerModelTrace(modelA); err != nil {
		t.Fatal(err)
	}
	due := traceBase.Add(15 * time.Minute)
	s.scheduleTick(due)
	run := runOutcome(s, modelA)
	state, _ := s.Snapshot()
	if run.Outcome != "running" || run.DayRuns != 0 || len(state.Results) != 3 {
		t.Fatal("scheduler started over a running model", run.Outcome)
	}
	s.scheduleTick(due.Add(10 * time.Second))
	close(up.hold)
	waitTraceIdle(t, s, 3)
	if up.calls.Load() != 3 {
		t.Fatal("extra probes sent", up.calls.Load())
	}
}

func TestTraceScheduleConcurrentTicksReserveOnce(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA, modelB)
	setSched(s, traceBase, plan(modelA, modelB))
	due := traceBase.Add(15 * time.Minute)
	clk.Set(due)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.scheduleModel(modelA, due) }()
		go func() { defer wg.Done(); s.scheduleModel(modelB, due) }()
	}
	wg.Wait()
	waitTraceIdle(t, s, 6)
	if up.calls.Load() != 6 {
		t.Fatal("duplicate batches", up.calls.Load())
	}
}

func TestTraceScheduleDoesNotTouchMilkGate(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA)
	s.Action(modelA, "hold")
	s.Action(modelA, "allow")
	gateBefore, _ := os.ReadFile(s.gate)
	infoBefore, _ := os.Stat(s.gate)
	state, _ := s.Snapshot()
	targetBefore := *state.Targets[modelA]
	time.Sleep(20 * time.Millisecond)
	if err := setSched(s, traceBase, plan(modelA)); err != nil {
		t.Fatal(err)
	}
	tickAt(t, s, clk, traceBase.Add(15*time.Minute), 3)
	state, _ = s.Snapshot()
	gateAfter, _ := os.ReadFile(s.gate)
	infoAfter, _ := os.Stat(s.gate)
	if !bytes.Equal(gateBefore, gateAfter) || !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatal("gate file rewritten")
	}
	if !reflect.DeepEqual(targetBefore, *state.Targets[modelA]) {
		t.Fatal("target state changed by a scheduled trace")
	}
}

func TestTraceScheduleResaveKeepsStateAndAnchorsNewModelsOnly(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: traceBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA, modelB)
	setSched(s, traceBase, plan(modelA))
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, s, clk, at, 3)
	before := runOutcome(s, modelA)
	later := at.Add(5 * time.Minute)
	if err := setSched(s, later, plan(modelA, modelB)); err != nil {
		t.Fatal(err)
	}
	if got := runOutcome(s, modelA); !reflect.DeepEqual(got, before) {
		t.Fatalf("unchanged plan altered: %+v vs %+v", got, before)
	}
	if got := runOutcome(s, modelB); !got.Attempt.Equal(later) {
		t.Fatalf("new model not anchored at enable time: %+v", got)
	}
	s.scheduleTick(later.Add(14 * time.Minute))
	if up.calls.Load() != 3 {
		t.Fatal("newly added model ran before its first baseline")
	}
}

func TestTraceScheduleHTTP(t *testing.T) {
	s := scheduledService(t, t.TempDir(), "http://127.0.0.1:1", nil, modelA)
	h := s.Handler("secret")
	post := func(path string, auth bool, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("/api/trace-schedule", false, `{"enabled":true}`); w.Code != 401 {
		t.Fatal("unauthenticated schedule change", w.Code)
	}
	if w := post("/api/trace-schedule", true, `{"enabled":true,"models":{"a/ghost":{}}}`); w.Code != 400 {
		t.Fatal("unknown model accepted", w.Code)
	}
	if w := post("/api/trace-schedule", true, `{"enabled":true,"models":{"`+modelA+`":{"interval_minutes":120,"daily_cap":2}}}`); w.Code != 400 {
		t.Fatal("a custom interval must be rejected", w.Code)
	}
	if w := post("/api/trace-schedule", true, `{"enabled":true,"models":{"`+modelA+`":{"interval_minutes":15,"daily_cap":6}}}`); w.Code != 200 {
		t.Fatal("valid schedule rejected", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/state", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"trace_schedule_status"`) || !strings.Contains(body, `"cadence_minutes":15`) || !strings.Contains(body, "不能证明") || strings.Contains(body, s.state.Alias) {
		t.Fatal("state lacks read-only schedule status", body)
	}
}

func TestTraceScheduleLoopStartsRunsAndStops(t *testing.T) {
	up := newUpstream(t)
	s := scheduledService(t, t.TempDir(), up.url, nil, modelA)
	s.StartTraceSchedule()
	s.StartTraceSchedule()
	time.Sleep(50 * time.Millisecond)
	if up.calls.Load() != 0 {
		t.Fatal("idle scheduler acted")
	}
	if err := s.SetTraceSchedule(true, plan(modelA)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if up.calls.Load() != 0 {
		t.Fatal("enabling must not start a run before the first baseline")
	}
	// Pretend the anchor is old enough that the model is due; the live loop must pick it up.
	s.mu.Lock()
	s.state.TraceSchedule.Runs[modelA].Attempt = time.Now().Add(-20 * time.Minute)
	s.mu.Unlock()
	s.wakeScheduler()
	waitTraceIdle(t, s, 3)
	if up.calls.Load() != 3 {
		t.Fatal("wake did not start the scheduled run", up.calls.Load())
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler goroutine did not stop")
	}
}

// The old service.py schedule() returned pending_unresolved for an unconfirmed batch and
// never retried it. The pause must hold across ticks and restarts and show why, until a
// conclusive full batch resolves it and the old cadence resumes.
func TestTraceScheduleUnknownStaysPausedUntilConclusiveBatch(t *testing.T) {
	up := newUpstream(t)
	up.set("short")
	clk := &fakeClock{t: traceBase}
	dir := t.TempDir()
	first := scheduledService(t, dir, up.url, clk, modelA)
	setSched(first, traceBase, plan(modelA))
	at := traceBase.Add(15 * time.Minute)
	tickAt(t, first, clk, at, 3)
	if run := runOutcome(first, modelA); run.Class != classUnknown || !run.Paused {
		t.Fatalf("%+v", run)
	}
	for _, d := range []time.Duration{time.Minute, 15 * time.Minute, time.Hour, 48 * time.Hour} {
		first.scheduleTick(at.Add(d))
	}
	if up.calls.Load() != 3 {
		t.Fatal("paused model ran again", up.calls.Load())
	}
	if st := first.TraceScheduleStatus(at.Add(time.Hour))[0]; !st.Paused || st.PauseReason == "" || !st.NextAt.IsZero() || !st.NextDue.IsZero() {
		t.Fatalf("status must show the pause, not a next due: %+v", st)
	}
	first.Close()

	second, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.clock = clk.Now
	second.scheduleTick(at.Add(24 * time.Hour))
	if up.calls.Load() != 3 || !runOutcome(second, modelA).Paused {
		t.Fatal("restart lifted the pause")
	}

	up.set("astra")
	clk.Set(at.Add(time.Hour))
	if err := second.TriggerModelTrace(modelA); err != nil {
		t.Fatal(err)
	}
	waitTraceIdle(t, second, 6)
	run := runOutcome(second, modelA)
	if run.Paused || run.Streak != 1 {
		t.Fatalf("conclusive batch did not resume: %+v", run)
	}
	resumed := at.Add(time.Hour)
	if st := second.TraceScheduleStatus(resumed)[0]; st.Paused || !st.NextAt.Equal(resumed.Add(30*time.Minute)) {
		t.Fatalf("%+v", st)
	}
	second.scheduleTick(resumed.Add(29 * time.Minute))
	if up.calls.Load() != 6 {
		t.Fatal("ran before the old cadence step")
	}
	tickAt(t, second, clk, resumed.Add(30*time.Minute), 9)
}
