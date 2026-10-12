package check

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Facts exported from the old collector's SQLite on 2026-10-12 (see README). They are
// test input only: the code derives every due time from them with the cadence rules.
func legacyFixtureSeed() TraceSeed {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			panic(err)
		}
		return v
	}
	return TraceSeed{Version: 1, Source: "magpie-modeltrace/service.py", ExportedAt: at("2026-10-12T08:43:54Z"), Entries: []TraceSeedEntry{
		{Model: modelA, Account: "sha256:ca704414", Expect: "gpt-6-astra", Status: "match", Prediction: "gpt-6-astra", Streak: 8,
			LastMatchAt: at("2026-10-12T06:49:25.662501888Z"), ResultAt: at("2026-10-12T06:49:25.662501888Z"), AttemptAt: at("2026-10-12T06:47:15.343375872Z")},
		{Model: modelB, Account: "sha256:8b686a70", Expect: "gpt-6-astra", Status: "match", Prediction: "gpt-6-astra", Streak: 34,
			LastMatchAt: at("2026-10-12T07:38:05.533310976Z"), ResultAt: at("2026-10-12T07:38:05.533310976Z"), AttemptAt: at("2026-10-12T07:35:37.072300032Z")},
	}}
}

var jst = time.FixedZone("JST", 9*3600)

func TestSeedDryRunReportsOldStatusWithoutWriting(t *testing.T) {
	var state State
	now := time.Date(2026, 10, 12, 8, 50, 0, 0, time.UTC) // 17:50 JST, when status.md was read
	report, err := ApplyTraceSeed(&state, legacyFixtureSeed(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.TraceSchedule.Runs) != 0 {
		t.Fatal("dry run wrote state")
	}
	want := map[string]struct {
		streak int
		due    string
	}{modelA: {8, "10-12 19:47"}, modelB: {34, "10-12 20:35"}}
	for _, r := range report {
		w := want[r.Model]
		if r.Action != "would_import" || r.Streak != w.streak || r.CadenceMinutes != 240 || r.DueAt.In(jst).Format("01-02 15:04") != w.due {
			t.Fatalf("%+v, want streak %d due %s JST at 4h", r, w.streak, w.due)
		}
	}
}

func TestSeedApplyIsIdempotentAndDoesNotEnableOrRun(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: time.Date(2026, 10, 12, 8, 50, 0, 0, time.UTC)}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA, modelB)
	before, _ := s.Snapshot()
	report, err := s.ImportTraceSeed(legacyFixtureSeed(), true)
	if err != nil || len(report) != 2 || report[0].Action != "imported" {
		t.Fatal(report, err)
	}
	once, _ := s.Snapshot()
	if once.TraceSchedule.Enabled || len(once.TraceSchedule.Models) != 0 || !reflect.DeepEqual(before.Targets, once.Targets) {
		t.Fatal("import must neither enable the schedule nor touch targets")
	}
	run := once.TraceSchedule.Runs[modelA]
	if run.Streak != 8 || run.Class != classMatch || run.Seed == "" || run.Fingerprint == nil || run.Fingerprint.Prediction != "gpt-6-astra" {
		t.Fatalf("%+v", run)
	}
	again, err := s.ImportTraceSeed(legacyFixtureSeed(), true)
	if err != nil || again[0].Action != "already_imported" || again[1].Action != "already_imported" {
		t.Fatal(again, err)
	}
	twice, _ := s.Snapshot()
	if !reflect.DeepEqual(once.TraceSchedule, twice.TraceSchedule) {
		t.Fatal("second import changed state")
	}
	s.scheduleTick(clk.Now().Add(24 * time.Hour))
	if up.calls.Load() != 0 {
		t.Fatal("a disabled schedule sent requests after import")
	}
}

// After import and enabling, the models are due at the old collector's own times and not
// one baseline after the enable click.
func TestSeedThenEnableKeepsOldDueTimes(t *testing.T) {
	up := newUpstream(t)
	clk := &fakeClock{t: time.Date(2026, 10, 12, 8, 50, 0, 0, time.UTC)}
	s := scheduledService(t, t.TempDir(), up.url, clk, modelA, modelB)
	if _, err := s.ImportTraceSeed(legacyFixtureSeed(), true); err != nil {
		t.Fatal(err)
	}
	if err := setSched(s, clk.Now(), plan(modelA, modelB)); err != nil {
		t.Fatal(err)
	}
	if got := runOutcome(s, modelA); !got.Attempt.Equal(legacyFixtureSeed().Entries[0].AttemptAt) {
		t.Fatal("enabling replaced the imported anchor", got.Attempt)
	}
	for _, st := range s.TraceScheduleStatus(clk.Now()) {
		want := map[string]string{modelA: "10-12 19:47", modelB: "10-12 20:35"}[st.Model]
		if st.NextAt.In(jst).Format("01-02 15:04") != want || st.CadenceMinutes != 240 || st.Streak == 0 || !st.Imported {
			t.Fatalf("%+v, want next %s JST", st, want)
		}
	}
	tickAt(t, s, clk, time.Date(2026, 10, 12, 10, 47, 15, 0, time.UTC), 0) // one second before A is due
	if up.calls.Load() != 0 {
		t.Fatal("ran before the old due time")
	}
	tickAt(t, s, clk, time.Date(2026, 10, 12, 10, 47, 16, 0, time.UTC), 3)
	a := runOutcome(s, modelA)
	if a.Streak != 9 || a.Class != classMatch || up.calls.Load() != 3 || runOutcome(s, modelB).Streak != 34 {
		t.Fatalf("model A should continue its streak and B must wait: %+v", a)
	}
	tickAt(t, s, clk, time.Date(2026, 10, 12, 11, 35, 38, 0, time.UTC), 6)
	if b := runOutcome(s, modelB); b.Streak != 35 {
		t.Fatalf("model B: %+v", b)
	}
	// Restart-safe: the reservation and the cadence anchor are persisted.
	if a = runOutcome(s, modelA); !a.Attempt.Equal(time.Date(2026, 10, 12, 10, 47, 16, 0, time.UTC)) {
		t.Fatalf("anchor %v", a.Attempt)
	}
}

func TestSeedNeverOverwritesNewerNativeState(t *testing.T) {
	var state State
	state.TraceSchedule.Runs = map[string]*TraceRun{modelA: {Attempt: legacyFixtureSeed().Entries[0].AttemptAt.Add(time.Minute), Streak: 1, Class: classMatch}}
	before := *state.TraceSchedule.Runs[modelA]
	report, err := ApplyTraceSeed(&state, legacyFixtureSeed(), time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report[0].Action != "skipped_newer_state" || report[1].Action != "imported" || !reflect.DeepEqual(*state.TraceSchedule.Runs[modelA], before) {
		t.Fatalf("%+v", report)
	}
	// A fresher export of the same account replaces the older import.
	newer := legacyFixtureSeed()
	newer.Entries = newer.Entries[1:]
	newer.Entries[0].AttemptAt = newer.Entries[0].AttemptAt.Add(4 * time.Hour)
	newer.Entries[0].ResultAt = newer.Entries[0].ResultAt.Add(4 * time.Hour)
	newer.Entries[0].LastMatchAt = newer.Entries[0].LastMatchAt.Add(4 * time.Hour)
	newer.Entries[0].Streak = 35
	if report, _ = ApplyTraceSeed(&state, newer, time.Now(), true); report[0].Action != "imported" || state.TraceSchedule.Runs[modelB].Streak != 35 {
		t.Fatalf("%+v", report)
	}
}

func TestSeedValidationLeavesStateUntouched(t *testing.T) {
	base := legacyFixtureSeed()
	mutate := func(f func(s *TraceSeed)) TraceSeed {
		s := legacyFixtureSeed()
		f(&s)
		return s
	}
	for name, seed := range map[string]TraceSeed{
		"version":         mutate(func(s *TraceSeed) { s.Version = 2 }),
		"empty":           mutate(func(s *TraceSeed) { s.Entries = nil }),
		"duplicate":       mutate(func(s *TraceSeed) { s.Entries = append(s.Entries, s.Entries[0]) }),
		"no model":        mutate(func(s *TraceSeed) { s.Entries[0].Model = "" }),
		"no attempt time": mutate(func(s *TraceSeed) { s.Entries[0].AttemptAt = time.Time{} }),
		"negative streak": mutate(func(s *TraceSeed) { s.Entries[0].Streak = -1 }),
		"match w/o time":  mutate(func(s *TraceSeed) { s.Entries[0].LastMatchAt = time.Time{} }),
		"match streak 0":  mutate(func(s *TraceSeed) { s.Entries[0].Streak = 0 }),
		"mismatch streak": mutate(func(s *TraceSeed) { s.Entries[0].Status = "mismatch" }),
		"unknown status":  mutate(func(s *TraceSeed) { s.Entries[0].Status = "ok" }),
		"529 w/o count":   mutate(func(s *TraceSeed) { s.Entries[0].Status, s.Entries[0].Streak = "degraded", 0 }),
		"second invalid":  mutate(func(s *TraceSeed) { s.Entries[1].Streak = -3 }),
	} {
		var state State
		if _, err := ApplyTraceSeed(&state, seed, time.Now(), true); err == nil {
			t.Fatal("accepted", name)
		}
		if len(state.TraceSchedule.Runs) != 0 {
			t.Fatal("partial import after error:", name)
		}
	}
	if base.Entries[0].Digest() == base.Entries[1].Digest() || base.Entries[0].Digest() != legacyFixtureSeed().Entries[0].Digest() {
		t.Fatal("digest unstable")
	}
}

func TestSeedDegradedAndStaleStates(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	seed := TraceSeed{Version: 1, Entries: []TraceSeedEntry{
		{Model: modelA, Status: "degraded", Streak529: 2, At529: now.Add(-30 * time.Minute), ResultAt: now.Add(-30 * time.Minute), AttemptAt: now.Add(-32 * time.Minute)},
		{Model: modelB, Status: "match", Streak: 8, LastMatchAt: now.Add(-10 * time.Hour), ResultAt: now.Add(-10 * time.Hour), AttemptAt: now.Add(-10 * time.Hour)},
	}}
	var state State
	report, err := ApplyTraceSeed(&state, seed, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report[0].DueAt.Equal(now.Add(-30*time.Minute).Add(2*time.Hour)) || state.TraceSchedule.Runs[modelA].Class != class529 {
		t.Fatalf("529 seed: %+v", report[0])
	}
	if report[1].CadenceMinutes != 15 || !report[1].DueAt.Equal(now.Add(-10*time.Hour).Add(15*time.Minute)) {
		t.Fatalf("a 10h old streak must be stale: %+v", report[1])
	}
}

func TestSeedHTTP(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 12, 8, 50, 0, 0, time.UTC)}
	s := scheduledService(t, t.TempDir(), "http://127.0.0.1:1", clk, modelA)
	h := s.Handler("secret")
	body, _ := json.Marshal(map[string]any{"seed": legacyFixtureSeed(), "apply": true})
	post := func(auth bool, payload []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/trace-seed", strings.NewReader(string(payload)))
		if auth {
			r.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(false, body); w.Code != 401 {
		t.Fatal("unauthenticated seed import", w.Code)
	}
	if w := post(true, []byte(`{"seed":{"version":9,"entries":[]},"apply":true}`)); w.Code != 400 {
		t.Fatal("invalid seed accepted", w.Code)
	}
	if w := post(true, body); w.Code != 200 || !strings.Contains(w.Body.String(), `"imported"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := post(true, body); !strings.Contains(w.Body.String(), "already_imported") {
		t.Fatal("not idempotent over HTTP", w.Body.String())
	}
}
