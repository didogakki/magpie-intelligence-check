package check

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// The account identifiers below are synthetic. Their sha256 prefixes are what the model IDs
// carry, exactly as for the real accounts.
const (
	compatAlice  = "alice@example.test" // sha256 69b1145a…
	compatBob    = "bob@example.test"   // sha256 25a2a844…
	compatModelA = "magpie-252-69b1145a/codex/gpt-6-astra"
	compatModelB = "magpie-252-25a2a844/codex/gpt-6-astra"
)

var (
	compatBase    = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	recordKeys    = []string{"provider", "source", "account", "plan", "model", "mismatch", "at", "expiresAt", "lastAttemptFailed", "lastAttemptAt", "lastAttemptStatus", "degraded", "degradedReason", "degradedAt", "retryAfter"}
	snapshotKeys  = []string{"version", "generatedAt", "records", "historyCount"}
	compatAccount = map[string]string{compatModelA: compatAlice, compatModelB: compatBob}
)

// compatEnv is a stand-in for the user's home: the "old state directory" sits where the old
// widget proxy reads it, with the old collector's service.lock next to the snapshot.
type compatEnv struct {
	t    *testing.T
	home string
	old  string // fake ~/.local/state/magpie-modeltrace
	clk  *fakeClock
	up   *traceUpstream
	s    *Service
}

func newCompatEnv(t *testing.T) *compatEnv {
	t.Helper()
	home := t.TempDir()
	old := filepath.Join(home, ".local/state/magpie-modeltrace")
	for _, d := range []string{old, filepath.Join(home, ".config/magpie")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".config/magpie/modeltrace_records_key"), []byte(strings.Repeat("k", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "service.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// What the old collector left behind: both accounts fresh for another 5 hours.
	prior := compatSnapshot{Version: 1, GeneratedAt: unixFloat(compatBase), HistoryCount: 162}
	for _, a := range []string{compatAlice, compatBob} {
		prior.Records = append(prior.Records, compatRecord{Provider: "codex", Source: "252", Account: a, Plan: "plus", Model: "gpt-6-astra",
			At: unixFloat(compatBase.Add(-time.Hour)), ExpiresAt: unixFloat(compatBase.Add(5 * time.Hour)), LastAttemptAt: unixFloat(compatBase.Add(-time.Hour)), LastAttemptStatus: ptr("match")})
	}
	raw, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(old, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	up := newUpstream(t)
	clk := &fakeClock{t: compatBase}
	s := scheduledService(t, t.TempDir(), up.url, clk, compatModelA, compatModelB)
	return &compatEnv{t: t, home: home, old: old, clk: clk, up: up, s: s}
}

func (e *compatEnv) seed(opts ...func(*TraceSeed)) TraceSeed {
	attempt := compatBase.Add(-time.Hour)
	// B trails A by 10 minutes, so the two models never run their batches at the same moment
	// (the fixture upstream hands out its three archived answers in call order).
	entry := func(model, hash string, offset time.Duration) TraceSeedEntry {
		at := attempt.Add(offset)
		return TraceSeedEntry{Model: model, Account: "sha256:" + hash, Expect: "gpt-6-astra", Status: "match", Prediction: "gpt-6-astra", Streak: 8, LastMatchAt: at, ResultAt: at, AttemptAt: at}
	}
	seed := TraceSeed{Version: 1, Source: "magpie-modeltrace/service.py", ExportedAt: compatBase, Entries: []TraceSeedEntry{entry(compatModelA, "69b1145a", 0), entry(compatModelB, "25a2a844", 10*time.Minute)},
		LegacySnapshot: &LegacySnapshotSeed{Enabled: true, StateDir: e.old, HistoryCount: 162, Targets: []LegacySnapshotTarget{
			{Model: compatModelA, Account: compatAlice, Plan: "plus"}, {Model: compatModelB, Account: compatBob, Plan: "plus"}}}}
	for _, o := range opts {
		o(&seed)
	}
	return seed
}

func (e *compatEnv) enable() {
	e.t.Helper()
	if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
		e.t.Fatal(err)
	}
	if err := setSched(e.s, e.clk.Now(), plan(compatModelA, compatModelB)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *compatEnv) snapshotPath() string { return filepath.Join(e.old, "snapshot.json") }

func (e *compatEnv) read() (compatSnapshot, []byte) {
	e.t.Helper()
	raw, err := os.ReadFile(e.snapshotPath())
	if err != nil {
		e.t.Fatal(err)
	}
	var snap compatSnapshot
	if err = json.Unmarshal(raw, &snap); err != nil {
		e.t.Fatal(err)
	}
	return snap, raw
}

func (e *compatEnv) refresh() { e.s.refreshLegacyCompat(e.clk.Now()) }

// untouched asserts that nothing in the old directory changed, bytes and mtimes alike.
func (e *compatEnv) untouched(before map[string]string, why string) {
	e.t.Helper()
	if after := dirSnapshot(e.t, e.old); fmt.Sprint(before) != fmt.Sprint(after) {
		e.t.Fatalf("%s: the old state directory was modified:\n%v\n%v", why, before, after)
	}
}

// widgetReader runs the real, unmodified /api/records handler of the installed widget
// proxy against this env's snapshot, with its clock pinned to `now`. Skipped if the widget
// is not installed on this machine.
func (e *compatEnv) widgetReader(now time.Time) []map[string]any {
	e.t.Helper()
	proxy := os.Getenv("MAGPIE_WIDGET_PROXY")
	if proxy == "" {
		h, _ := os.UserHomeDir()
		proxy = filepath.Join(h, ".local/opt/magpie-widget/proxy.py")
	}
	py, err := exec.LookPath("python3")
	if _, serr := os.Stat(proxy); err != nil || serr != nil {
		e.t.Skip("widget proxy.py or python3 not available")
	}
	script := filepath.Join(e.t.TempDir(), "reader.py")
	if err = os.WriteFile(script, []byte(readerScript), 0o600); err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", "-B", script, proxy, strconv.FormatFloat(unixFloat(now), 'f', 6, 64))
	cmd.Env = []string{"HOME=" + e.home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("reader harness: %v\n%s", err, out)
	}
	status, body, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if status != "200" {
		e.t.Fatalf("widget reader answered %s: %s", status, body)
	}
	var data struct {
		Version int              `json:"version"`
		Records []map[string]any `json:"records"`
	}
	if err = json.Unmarshal([]byte(body), &data); err != nil || data.Version != 1 {
		e.t.Fatal(err, body)
	}
	return data.Records
}

// readerScript loads the widget proxy's source unmodified (no bytecode written next to it)
// and issues one authenticated /api/records request, with time.time() pinned.
const readerScript = `import sys, threading, urllib.request, urllib.error
from http.server import ThreadingHTTPServer
proxy, now = sys.argv[1], float(sys.argv[2])
ns = {"__name__": "widget_proxy"}
exec(compile(open(proxy).read(), proxy, "exec"), ns)
class Clock:
    @staticmethod
    def time():
        return now
ns["time"] = Clock
srv = ThreadingHTTPServer(("127.0.0.1", 0), ns["Handler"])
threading.Thread(target=srv.serve_forever, daemon=True).start()
key = ns["KEYFILE"].read_text().strip()
req = urllib.request.Request("http://127.0.0.1:%d/api/records" % srv.server_port, headers={"Authorization": "Bearer " + key})
try:
    r = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=10)
    print(r.status); print(r.read().decode())
except urllib.error.HTTPError as e:
    print(e.code); print("")
srv.shutdown()
`

func byAccount(records []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range records {
		out[r["account"].(string)] = r
	}
	return out
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sorted(v []string) []string { c := append([]string(nil), v...); sort.Strings(c); return c }

func TestCompatSnapshotKeepsTheOldSchema(t *testing.T) {
	e := newCompatEnv(t)
	e.enable()
	e.refresh()
	_, raw := e.read()
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keysOf(top), sorted(snapshotKeys)) || top["version"] != float64(1) {
		t.Fatal("top-level fields:", keysOf(top))
	}
	recs := top["records"].([]any)
	if len(recs) != 2 {
		t.Fatal(len(recs))
	}
	for _, r := range recs {
		rec := r.(map[string]any)
		if !reflect.DeepEqual(keysOf(rec), sorted(recordKeys)) {
			t.Fatal("record fields:", keysOf(rec))
		}
		if rec["provider"] != "codex" || rec["source"] != "252" || rec["model"] != "gpt-6-astra" || rec["plan"] != "plus" || rec["mismatch"] != false ||
			rec["degraded"] != false || rec["degradedReason"] != nil || rec["degradedAt"] != nil || rec["retryAfter"] != nil || rec["lastAttemptFailed"] != false || rec["lastAttemptStatus"] != "match" {
			t.Fatalf("%v", rec)
		}
		if _, ok := rec["at"].(float64); !ok {
			t.Fatal("at must be a number")
		}
	}
	if top["historyCount"] != float64(162) {
		t.Fatal("history count starts at the old collector's total:", top["historyCount"])
	}
	st, err := os.Stat(e.snapshotPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("snapshot mode", st.Mode(), err)
	}
	// Nothing but the permitted fields: no prompt, answer or credential anywhere in either file.
	status, _ := os.ReadFile(filepath.Join(e.old, "status.md"))
	for _, file := range [][]byte{raw, status} {
		for _, secret := range []string{"fixture", "gateway", "quality-probe", e.s.state.Alias} {
			if secret != "" && strings.Contains(string(file), secret) {
				t.Fatalf("snapshot leaks %q", secret)
			}
		}
	}
}

// Account identifiers are needed in the old snapshot, and nowhere else.
func TestCompatAccountsStayOutOfEveryOtherSurface(t *testing.T) {
	e := newCompatEnv(t)
	report, err := e.s.ImportTraceSeed(e.seed(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = setSched(e.s, compatBase, plan(compatModelA, compatModelB)); err != nil {
		t.Fatal(err)
	}
	e.refresh()
	state, health := e.s.Snapshot()
	api := e.s.Handler("secret")
	r := httptestGet(api, "/api/state")
	surfaces := map[string]string{
		"import report": mustJSON(report), "report summary": mustJSON(LegacySnapshotReport(e.seed())), "state snapshot": mustJSON(state), "health": health,
		"compat status": mustJSON(e.s.LegacyCompatStatus()), "schedule status": mustJSON(e.s.TraceScheduleStatus(compatBase)), "GET /api/state": r,
	}
	for name, v := range surfaces {
		for _, account := range []string{compatAlice, compatBob} {
			if strings.Contains(v, account) {
				t.Fatalf("%s exposes an old account identifier", name)
			}
		}
	}
	if !strings.Contains(r, `"legacy_compat"`) || !strings.Contains(r, `"active":true`) {
		t.Fatal("the console should report the writer state:", r)
	}
	// Errors never echo the account either.
	bad := e.seed(func(s *TraceSeed) { s.LegacySnapshot.Targets[0].Account = "mallory@example.test" })
	if _, err = e.s.ImportTraceSeed(bad, false); err == nil || strings.Contains(err.Error(), "mallory") {
		t.Fatal("mismatched account accepted or echoed:", err)
	}
	status, _ := os.ReadFile(filepath.Join(e.old, "status.md"))
	for _, account := range []string{compatAlice, compatBob} {
		if strings.Contains(string(status), account) {
			t.Fatal("status.md must mask accounts like the old one did")
		}
	}
	if !strings.Contains(string(status), "al***@example.test #69b1 / plus") || !strings.Contains(string(status), "bo***@example.test #25a2 / plus") {
		t.Fatal("status.md rows:", string(status))
	}
}

func httptestGet(h http.Handler, path string) string {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Body.String()
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCompatStatusMaskMatchesTheOldCollector(t *testing.T) {
	// Expected values computed with the old service.py _mask() on these inputs.
	for in, want := range map[[2]string]string{
		{"alice@example.test", "plus"}: "al***@example.test #69b1 / plus",
		{"é名@x y.test", "plus"}:        "??***@x?y.test #d42b / plus",
		{"nodomain", "plus"}:           "no*** #0bb9 / plus",
	} {
		if got := statusMask(in[0], in[1]); got != want {
			t.Fatalf("%q: %q, want %q", in[0], got, want)
		}
	}
}

// The core promise: the old widget's own reader keeps getting fresh records after the old
// collector is gone, through scheduled matches, a 529 episode, and the recovery from it.
func TestCompatOldWidgetReaderStaysFreshOverTime(t *testing.T) {
	e := newCompatEnv(t)
	e.enable()
	e.refresh()
	for _, rec := range e.widgetReader(compatBase) {
		if rec["lastAttemptStatus"] != "match" {
			t.Fatal(rec)
		}
	}
	lag := map[string]time.Duration{compatAlice: 0, compatBob: 10 * time.Minute}
	accounts := []string{compatAlice, compatBob}
	at, results, batches := compatBase.Add(3*time.Hour), 0, 0 // A: seeded attempt + 4h cadence
	// step runs one batch per model (B 10 minutes after A) and refreshes the writer once, as the live loop would.
	step := func(at time.Time) {
		t.Helper()
		for _, a := range accounts {
			results += 3
			tickAt(t, e.s, e.clk, at.Add(lag[a]), results)
			batches++
		}
		e.refresh()
		if snap, _ := e.read(); snap.HistoryCount != 162+batches {
			t.Fatalf("historyCount %d, want %d", snap.HistoryCount, 162+batches)
		}
	}
	for i := 0; i < 6; i++ { // 24 hours at the 4 hour steady cadence
		step(at)
		// Right after, and just before the next batch of that model, the reader has both records, from this batch.
		for _, a := range accounts {
			for _, now := range []time.Time{at.Add(lag[a]), at.Add(lag[a] + 4*time.Hour - time.Second)} {
				rec := byAccount(e.widgetReader(now))[a]
				done := at.Add(lag[a])
				if rec == nil || rec["at"].(float64) != unixFloat(done) || rec["expiresAt"].(float64) != unixFloat(done.Add(6*time.Hour)) || rec["degraded"] != false || rec["lastAttemptStatus"] != "match" {
					t.Fatalf("cycle %d, reader at %v: %v for %s", i+1, now, rec, a)
				}
			}
		}
		at = at.Add(4 * time.Hour)
	}
	// Without a writer the old file simply ages out: this is what stopping the old collector does.
	if got := e.widgetReader(at.Add(3 * time.Hour)); len(got) != 0 {
		t.Fatalf("expected the unrefreshed snapshot to expire after 6h, reader has %v", got)
	}

	// 529 episode: active marker, no TTL, fixed finite expiresAt; the record's time stays that of the last scored result.
	e.up.set("529")
	step(at)
	for _, a := range accounts {
		rec := byAccount(e.widgetReader(at.Add(time.Hour)))[a]
		hit, lastOK := at.Add(lag[a]), at.Add(lag[a]-4*time.Hour)
		if rec == nil || rec["degraded"] != true || rec["degradedReason"] != "http_529" || rec["degradedAt"].(float64) != unixFloat(hit) || rec["retryAfter"].(float64) != unixFloat(hit.Add(time.Hour)) ||
			rec["expiresAt"].(float64) != compatDegradedExpiresAt || rec["at"].(float64) != unixFloat(lastOK) || rec["mismatch"] != false ||
			rec["lastAttemptFailed"] != true || rec["lastAttemptStatus"] != "degraded" || rec["lastAttemptAt"].(float64) != unixFloat(hit) {
			t.Fatalf("529 record for %s: %v", a, rec)
		}
	}
	// The marker outlives the result's own 6 hours; the reader keeps showing it.
	if got := e.widgetReader(at.Add(30 * time.Hour)); len(got) != 2 {
		t.Fatal("an active 529 marker must not expire:", got)
	}
	// Second 529 after the 1h wait: the next retry is 2h out.
	at = at.Add(time.Hour)
	step(at)
	if rec := byAccount(e.widgetReader(at.Add(time.Hour)))[compatAlice]; rec["retryAfter"].(float64) != unixFloat(at.Add(2*time.Hour)) || rec["lastAttemptAt"].(float64) != unixFloat(at) {
		t.Fatalf("second 529: %v", rec)
	}
	// Recovery: a scored batch ends the marker; the record is a fresh 6h match again.
	e.up.set("astra")
	at = at.Add(2 * time.Hour)
	step(at)
	for _, a := range accounts {
		done := at.Add(lag[a])
		rec := byAccount(e.widgetReader(done))[a]
		if rec == nil || rec["degraded"] != false || rec["degradedAt"] != nil || rec["retryAfter"] != nil || rec["at"].(float64) != unixFloat(done) ||
			rec["expiresAt"].(float64) != unixFloat(done.Add(6*time.Hour)) || rec["lastAttemptFailed"] != false {
			t.Fatalf("recovery for %s: %v", a, rec)
		}
	}
}

func TestCompatRecordSemantics(t *testing.T) {
	target := LegacyCompatTarget{Account: compatAlice, Plan: "plus", Expect: "gpt-6-astra"}
	now := compatBase
	fp := func(ago time.Duration, prediction string) *TraceFingerprint {
		return &TraceFingerprint{Expect: "gpt-6-astra", Prediction: prediction, At: now.Add(-ago)}
	}
	cases := []struct {
		name string
		run  *TraceRun
		none bool
		want func(compatRecord) bool
	}{
		{"fresh match", &TraceRun{Class: classMatch, ClassAt: now.Add(-time.Hour), Fingerprint: fp(time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return !r.Mismatch && r.At == unixFloat(now.Add(-time.Hour)) && r.ExpiresAt == unixFloat(now.Add(5*time.Hour)) && !r.LastAttemptFailed && *r.LastAttemptStatus == "match" && r.LastAttemptAt == r.At && !r.Degraded
		}},
		{"match exactly 6h old is still shown", &TraceRun{Class: classMatch, Fingerprint: fp(6*time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool { return r.ExpiresAt == unixFloat(now) }},
		{"match older than 6h", &TraceRun{Class: classMatch, Fingerprint: fp(6*time.Hour+time.Second, "gpt-6-astra")}, true, nil},
		{"mismatch", &TraceRun{Class: classMismatch, Fingerprint: fp(time.Hour, "gpt-5.6-luna")}, false, func(r compatRecord) bool { return r.Mismatch && *r.LastAttemptStatus == "mismatch" && !r.Degraded }},
		{"nothing known", &TraceRun{}, true, nil},
		{"failure after a fresh match", &TraceRun{Class: classTransient, ClassAt: now.Add(-10 * time.Minute), Fingerprint: fp(time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return r.At == unixFloat(now.Add(-time.Hour)) && r.ExpiresAt == unixFloat(now.Add(5*time.Hour)) && r.LastAttemptFailed && *r.LastAttemptStatus == "failed" && r.LastAttemptAt == unixFloat(now.Add(-10*time.Minute)) && !r.Degraded
		}},
		{"unknown after a fresh match", &TraceRun{Class: classUnknown, ClassAt: now.Add(-10 * time.Minute), Fingerprint: fp(time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return r.LastAttemptFailed && *r.LastAttemptStatus == "failed"
		}},
		{"failure after an expired match is no record", &TraceRun{Class: classUnknown, ClassAt: now.Add(-time.Minute), Fingerprint: fp(7*time.Hour, "gpt-6-astra")}, true, nil},
		{"529 over a fresh match", &TraceRun{Class: class529, ClassAt: now.Add(-time.Minute), Streak529: 2, At529: now.Add(-time.Minute), Fingerprint: fp(2*time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return r.Degraded && *r.DegradedReason == "http_529" && *r.DegradedAt == unixFloat(now.Add(-time.Minute)) && *r.RetryAfter == unixFloat(now.Add(-time.Minute+2*time.Hour)) &&
				r.ExpiresAt == compatDegradedExpiresAt && r.At == unixFloat(now.Add(-2*time.Hour)) && r.LastAttemptFailed && *r.LastAttemptStatus == "degraded" && !r.Mismatch
		}},
		{"529 without any fresh result is the record itself", &TraceRun{Class: class529, ClassAt: now.Add(-9 * time.Hour), Streak529: 1, At529: now.Add(-9 * time.Hour), Fingerprint: fp(20*time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return r.Degraded && r.At == unixFloat(now.Add(-9*time.Hour)) && r.ExpiresAt == compatDegradedExpiresAt && !r.Mismatch && *r.LastAttemptStatus == "degraded"
		}},
		{"529 wait is capped at 6h", &TraceRun{Class: class529, Streak529: 9, At529: now, ClassAt: now}, false, func(r compatRecord) bool { return *r.RetryAfter == unixFloat(now.Add(6*time.Hour)) }},
		{"unknown after 529 keeps the marker", &TraceRun{Class: classUnknown, ClassAt: now.Add(-time.Minute), Streak529: 1, At529: now.Add(-time.Hour), Fingerprint: fp(3*time.Hour, "gpt-6-astra")}, false, func(r compatRecord) bool {
			return r.Degraded && *r.DegradedAt == unixFloat(now.Add(-time.Hour)) && *r.LastAttemptStatus == "failed" && r.LastAttemptAt == unixFloat(now.Add(-time.Minute)) && r.ExpiresAt == compatDegradedExpiresAt
		}},
	}
	for _, c := range cases {
		rec, ok := compatRecordFor(target, c.run, now)
		if c.none {
			if ok {
				t.Errorf("%s: unexpected record %+v", c.name, rec)
			}
			continue
		}
		if !ok || !c.want(rec) {
			t.Errorf("%s: %+v", c.name, rec)
		}
	}
}

// An unrelated installation, a disabled schedule, a partly scheduled mapping or a running old
// collector must leave the old directory exactly as it is.
func TestCompatLeavesTheOldSnapshotUntouched(t *testing.T) {
	t.Run("unrelated install without opt-in", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if err := setSched(e.s, compatBase, plan(compatModelA)); err != nil {
			t.Fatal(err)
		}
		e.s.StartTraceSchedule() // the live loop must not write either
		tickAt(t, e.s, e.clk, compatBase.Add(20*time.Minute), 3)
		e.s.wakeScheduler()
		time.Sleep(50 * time.Millisecond)
		e.refresh()
		e.untouched(before, "no legacy_snapshot opt-in")
		if st := e.s.LegacyCompatStatus(); st.Configured || st.Active {
			t.Fatalf("%+v", st)
		}
	})
	t.Run("seed import without the section", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(func(s *TraceSeed) { s.LegacySnapshot = nil }), true); err != nil {
			t.Fatal(err)
		}
		if err := setSched(e.s, compatBase, plan(compatModelA, compatModelB)); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "seed without opt-in")
	})
	t.Run("dry run only validates", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(), false); err != nil {
			t.Fatal(err)
		}
		if err := setSched(e.s, compatBase, plan(compatModelA, compatModelB)); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "dry run")
		if e.s.LegacyCompatStatus().Configured {
			t.Fatal("a dry run configured the writer")
		}
	})
	t.Run("schedule disabled", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "import alone, schedule never enabled")
		e.enable()
		e.refresh()
		if e.s.LegacyCompatStatus().Writes != 1 {
			t.Fatal("enabled schedule should have written once")
		}
		if err := e.s.SetTraceSchedule(false, plan(compatModelA, compatModelB)); err != nil {
			t.Fatal(err)
		}
		afterOn := dirSnapshot(t, e.old)
		e.clk.Set(compatBase.Add(time.Hour))
		e.refresh()
		e.untouched(afterOn, "schedule switched off again")
		if lock, err := lockLegacyCollector(e.old); err != nil {
			t.Fatal("a disabled schedule must give the old lock back for a rollback:", err)
		} else {
			lock.Unlock()
		}
	})
	t.Run("one mapped model not scheduled or its target disabled", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
			t.Fatal(err)
		}
		if err := setSched(e.s, compatBase, plan(compatModelA)); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "model B not scheduled")
		if err := setSched(e.s, compatBase, plan(compatModelA, compatModelB)); err != nil {
			t.Fatal(err)
		}
		if err := e.s.SetTarget(compatModelB, false); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "model B target disabled")
		if err := e.s.SetTarget(compatModelB, true); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		if e.s.LegacyCompatStatus().Writes != 1 {
			t.Fatal("complete mapping should write", e.s.LegacyCompatStatus())
		}
	})
	t.Run("expect differs from the seed", func(t *testing.T) {
		e := newCompatEnv(t)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
			t.Fatal(err)
		}
		p := plan(compatModelA, compatModelB)
		p[compatModelA] = TraceModelPlan{IntervalMinutes: 15, DailyCap: 12, Expect: "gpt-5.6-luna"}
		if err := setSched(e.s, compatBase, p); err != nil {
			t.Fatal(err)
		}
		e.refresh()
		e.untouched(before, "expect mismatch")
		if st := e.s.LegacyCompatStatus(); st.Active || !strings.Contains(st.Reason, "期望候选") {
			t.Fatalf("%+v", st)
		}
	})
}

func TestCompatNeedsTheOldCollectorStoppedAndExcludesItWhileActive(t *testing.T) {
	e := newCompatEnv(t)
	running := flock.New(filepath.Join(e.old, "service.lock")) // the old collector's life-long lock
	if ok, err := running.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	before := dirSnapshot(t, e.old)
	if _, err := e.s.ImportTraceSeed(e.seed(), true); err == nil || !strings.Contains(err.Error(), "旧采集器仍在运行") {
		t.Fatalf("import with the old collector running: %v", err)
	}
	if _, err := e.s.ImportTraceSeed(e.seed(), false); err == nil {
		t.Fatal("the dry run must report the same blocker")
	}
	running.Unlock()
	e.enable()
	// The old collector comes back while the writer is active: it cannot take its lock and would exit.
	e.refresh()
	if _, err := lockLegacyCollector(e.old); err != errLegacyRunning {
		t.Fatal("the active writer must hold the old collector's lock:", err)
	}
	// Reverse order: the writer was enabled, the lock is lost (new process), the old collector runs: blocked, no write.
	e.s.releaseLegacyLock()
	running = flock.New(filepath.Join(e.old, "service.lock"))
	if ok, _ := running.TryLock(); !ok {
		t.Fatal("lock not released")
	}
	e.s.compat.mu.Lock()
	e.s.compat.lastBody = nil
	e.s.compat.mu.Unlock()
	before = dirSnapshot(t, e.old)
	e.clk.Set(compatBase.Add(time.Hour))
	if d := e.s.refreshLegacyCompat(e.clk.Now()); d != compatTick {
		t.Fatal("a blocked writer keeps checking:", d)
	}
	e.untouched(before, "old collector running")
	if st := e.s.LegacyCompatStatus(); st.Active || !strings.Contains(st.Reason, "旧采集器仍在运行") {
		t.Fatalf("%+v", st)
	}
	running.Unlock()
	e.clk.Set(compatBase.Add(2 * time.Hour))
	e.refresh()
	if st := e.s.LegacyCompatStatus(); !st.Active {
		t.Fatalf("%+v", st)
	}
	e.s.Close()
	if lock, err := lockLegacyCollector(e.old); err != nil {
		t.Fatal("Close must give the lock back:", err)
	} else {
		lock.Unlock()
	}
}

func TestCompatSeedValidation(t *testing.T) {
	e := newCompatEnv(t)
	mutate := map[string]func(*TraceSeed){
		"account does not hash to the model": func(s *TraceSeed) { s.LegacySnapshot.Targets[0].Account = "mallory@example.test" },
		"entry account hash differs":         func(s *TraceSeed) { s.Entries[0].Account = "sha256:deadbeef" },
		"two models for one account": func(s *TraceSeed) {
			s.LegacySnapshot.Targets[1].Account, s.LegacySnapshot.Targets[1].Model = compatAlice, compatModelA
		},
		"target not in entries":             func(s *TraceSeed) { s.LegacySnapshot.Targets[1].Model = "magpie-252-25a2a844/codex/other" },
		"missing target":                    func(s *TraceSeed) { s.LegacySnapshot.Targets = s.LegacySnapshot.Targets[:1] },
		"relative state dir":                func(s *TraceSeed) { s.LegacySnapshot.StateDir = "state" },
		"non-canonical state dir":           func(s *TraceSeed) { s.LegacySnapshot.StateDir += "/../x" },
		"state dir without a lock file":     func(s *TraceSeed) { s.LegacySnapshot.StateDir = t.TempDir() },
		"control characters in the account": func(s *TraceSeed) { s.LegacySnapshot.Targets[0].Account += "\n" },
		"scored entry without prediction":   func(s *TraceSeed) { s.Entries[0].Prediction = "" },
		"opt-out carrying fields":           func(s *TraceSeed) { s.LegacySnapshot.Enabled = false },
		"negative history count":            func(s *TraceSeed) { s.LegacySnapshot.HistoryCount = -1 },
		"entry without expect":              func(s *TraceSeed) { s.Entries[0].Expect = "" },
	}
	for name, m := range mutate {
		var state State
		_, err := ApplyTraceSeed(&state, e.seed(m), compatBase, true)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if state.Compat != nil || len(state.TraceSchedule.Runs) != 0 {
			t.Errorf("%s: partial state written", name)
		}
		for _, a := range []string{compatAlice, compatBob, "mallory"} {
			if strings.Contains(err.Error(), a) {
				t.Errorf("%s: error echoes an account: %v", name, err)
			}
		}
	}
}

func TestCompatRefusesToOverwriteWhatItDoesNotUnderstand(t *testing.T) {
	write := func(e *compatEnv, v any) {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(e.snapshotPath(), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	live := func(account string) map[string]any {
		return map[string]any{"account": account, "plan": "plus", "expiresAt": unixFloat(compatBase.Add(time.Hour)), "degraded": false}
	}
	for name, setup := range map[string]func(*compatEnv){
		"a live record of an unmapped account": func(e *compatEnv) {
			write(e, map[string]any{"version": 1, "records": []any{live(compatAlice), live("carol@example.test")}})
		},
		"a degraded record of an unmapped account": func(e *compatEnv) {
			r := live("carol@example.test")
			r["expiresAt"], r["degraded"] = 1, true
			write(e, map[string]any{"version": 1, "records": []any{r}})
		},
		"another schema version": func(e *compatEnv) { write(e, map[string]any{"version": 2, "records": []any{}}) },
		"not json":               func(e *compatEnv) { os.WriteFile(e.snapshotPath(), []byte("not json"), 0o600) },
		"a symlink": func(e *compatEnv) {
			target := filepath.Join(t.TempDir(), "elsewhere.json")
			os.WriteFile(target, []byte(`{"version":1,"records":[]}`), 0o600)
			os.Remove(e.snapshotPath())
			os.Symlink(target, e.snapshotPath())
		},
	} {
		e := newCompatEnv(t)
		setup(e)
		before := dirSnapshot(t, e.old)
		if _, err := e.s.ImportTraceSeed(e.seed(), true); err == nil {
			t.Errorf("%s: import accepted", name)
		}
		e.untouched(before, name)
	}
	// An expired record of an unmapped account is not live and does not block.
	e := newCompatEnv(t)
	expired := live("carol@example.test")
	expired["expiresAt"] = unixFloat(compatBase.Add(-time.Hour))
	write(e, map[string]any{"version": 1, "records": []any{expired}})
	if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
		t.Fatal(err)
	}
	// Absent snapshot is fine: created at the first write.
	e = newCompatEnv(t)
	os.Remove(e.snapshotPath())
	e.enable()
	e.refresh()
	if snap, _ := e.read(); len(snap.Records) != 2 {
		t.Fatal(snap)
	}
	// After activation, the file being swapped for a symlink or foreign content stops the writer.
	e = newCompatEnv(t)
	e.enable()
	e.refresh()
	os.Remove(e.snapshotPath())
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	os.WriteFile(target, []byte("keep"), 0o600)
	os.Symlink(target, e.snapshotPath())
	e.clk.Set(compatBase.Add(10 * time.Minute))
	e.s.compat.mu.Lock()
	e.s.compat.lastBody = nil
	e.s.compat.mu.Unlock()
	e.refresh()
	if raw, _ := os.ReadFile(target); string(raw) != "keep" {
		t.Fatal("wrote through a symlink")
	}
	if st := e.s.LegacyCompatStatus(); st.Active {
		t.Fatalf("%+v", st)
	}
}

func TestCompatOptOutAndReimport(t *testing.T) {
	e := newCompatEnv(t)
	e.enable()
	e.refresh()
	tickAt(t, e.s, e.clk, compatBase.Add(3*time.Hour), 3)
	if st := e.s.LegacyCompatStatus(); !st.Configured {
		t.Fatal(st)
	}
	e.s.mu.Lock()
	added := e.s.state.Compat.HistoryAdded
	e.s.mu.Unlock()
	if added != 1 {
		t.Fatal("history rows added since the import:", added)
	}
	// Importing the same seed again keeps the running count.
	if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	added = e.s.state.Compat.HistoryAdded
	e.s.mu.Unlock()
	if added != 1 {
		t.Fatal("re-import reset the count:", added)
	}
	// An explicit opt-out switches the writer off and returns the lock.
	off := e.seed(func(s *TraceSeed) { s.LegacySnapshot = &LegacySnapshotSeed{Enabled: false} })
	if _, err := e.s.ImportTraceSeed(off, true); err != nil {
		t.Fatal(err)
	}
	before := dirSnapshot(t, e.old)
	e.clk.Set(compatBase.Add(4 * time.Hour))
	e.refresh()
	e.untouched(before, "after opt-out")
	if lock, err := lockLegacyCollector(e.old); err != nil {
		t.Fatal(err)
	} else {
		lock.Unlock()
	}
	// The stored form survives a restart: it lives in the private state database.
	dir := e.s.dir
	e.s.Close()
	again, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.state.Compat != nil {
		t.Fatal("opt-out should have been stored")
	}
}

func TestCompatConfigSurvivesRestartAndLiveLoopWrites(t *testing.T) {
	e := newCompatEnv(t)
	if _, err := e.s.ImportTraceSeed(e.seed(), true); err != nil {
		t.Fatal(err)
	}
	dir := e.s.dir
	e.s.Close()
	s, err := New(dir, filepath.Join(dir, "gate.js"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.clock = e.clk.Now
	if s.state.Compat == nil || s.state.Compat.Targets[compatModelA].Account != compatAlice {
		t.Fatal("compat config lost over restart")
	}
	s.StartTraceSchedule()
	if err = setSched(s, compatBase, plan(compatModelA, compatModelB)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st := s.LegacyCompatStatus(); st.Writes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the live loop did not write", s.LegacyCompatStatus())
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := e.read()
	if len(snap.Records) != 2 || snap.GeneratedAt != unixFloat(compatBase) {
		t.Fatalf("%+v", snap)
	}
}

func TestCompatHeartbeatAndNoChurn(t *testing.T) {
	e := newCompatEnv(t)
	e.enable()
	e.refresh()
	first := dirSnapshot(t, e.old)
	e.clk.Set(compatBase.Add(2 * time.Minute))
	e.refresh()
	e.untouched(first, "nothing changed")
	e.clk.Set(compatBase.Add(compatHeartbeat + time.Second))
	e.refresh()
	snap, _ := e.read()
	if snap.GeneratedAt != unixFloat(compatBase.Add(compatHeartbeat+time.Second)) {
		t.Fatal("the heartbeat should refresh generatedAt", snap.GeneratedAt)
	}
	// An expiring result is pruned from the file at a heartbeat, like the old collector did on rebuild.
	e.clk.Set(compatBase.Add(5*time.Hour + time.Hour + time.Minute))
	e.refresh()
	if snap, _ = e.read(); len(snap.Records) != 0 {
		t.Fatal("expired records must be dropped:", snap.Records)
	}
}

func TestCompatSeedCarriesTheScoredResultUnderALaterFailure(t *testing.T) {
	var state State
	attempt := compatBase.Add(-30 * time.Minute)
	seed := TraceSeed{Version: 1, Source: "t", ExportedAt: compatBase, Entries: []TraceSeedEntry{{Model: compatModelA, Account: "sha256:69b1145a", Expect: "gpt-6-astra", Status: "degraded",
		Streak529: 1, At529: attempt, ResultAt: attempt, AttemptAt: attempt, ScoredAt: compatBase.Add(-2 * time.Hour), ScoredPrediction: "gpt-6-astra"}}}
	if _, err := ApplyTraceSeed(&state, seed, compatBase, true); err != nil {
		t.Fatal(err)
	}
	rec, ok := compatRecordFor(LegacyCompatTarget{Account: compatAlice, Plan: "plus", Expect: "gpt-6-astra"}, state.TraceSchedule.Runs[compatModelA], compatBase)
	if !ok || !rec.Degraded || rec.At != unixFloat(compatBase.Add(-2*time.Hour)) || rec.LastAttemptAt != unixFloat(attempt) || *rec.LastAttemptStatus != "degraded" {
		t.Fatalf("%+v", rec)
	}
	bad := seed
	bad.Entries = []TraceSeedEntry{seed.Entries[0]}
	bad.Entries[0].ScoredAt = compatBase // later than the result
	if _, err := ApplyTraceSeed(&state, bad, compatBase, false); err == nil {
		t.Fatal("a scored result newer than the newest result accepted")
	}
}

func TestCompatExportCarriesTheExactAccountOnlyOnRequest(t *testing.T) {
	f := newLegacyFixture(t)
	f.match(1 * time.Hour)
	f.row(2*time.Hour, "quota", "", "", "{}")
	f.row(3*time.Hour, "failed", "", f.log529(true), `{"stop_reason":"inference:unknown:http_529"}`)
	f.attempt(3*time.Hour-time.Minute, 0)
	// Another source's row still counts in the old historyCount (SELECT COUNT(*) FROM history).
	if _, err := f.db.Exec(`INSERT INTO history VALUES ('h','1','x','p','historical_unverified','failed','',' ','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "service.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.db.Close()
	now := f.base.Add(24 * time.Hour)

	plain, _, err := ExportLegacySeed(f.dir, []string{f.model}, "", now)
	if err != nil || plain.LegacySnapshot != nil || strings.Contains(mustJSON(plain), "legacy_snapshot") {
		t.Fatal("a plain export must not carry the opt-in or account:", err)
	}
	seed, _, err := ExportLegacySeedWithSnapshot(f.dir, []string{f.model}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	l := seed.LegacySnapshot
	if l == nil || !l.Enabled || l.StateDir != f.dir || l.HistoryCount != 4 || len(l.Targets) != 1 || l.Targets[0] != (LegacySnapshotTarget{Model: f.model, Account: f.account, Plan: "plus"}) {
		t.Fatalf("%+v", l)
	}
	e := seed.Entries[0]
	if e.Status != "degraded" || e.Streak529 != 1 || e.ScoredPrediction != "gpt-6-astra" || !e.ScoredAt.Equal(f.base.Add(time.Hour)) {
		t.Fatalf("%+v", e)
	}
	if _, err = checkLegacySnapshotSeed(l, seed.Entries, now, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ExportLegacySeedWithSnapshot("relative/dir", []string{f.model}, "", now); err == nil {
		t.Fatal("a relative state dir cannot be the snapshot location")
	}
}
