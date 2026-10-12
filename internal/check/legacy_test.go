package check

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyFixture builds a state directory in the layout of the old collector.
type legacyFixture struct {
	t       *testing.T
	dir     string
	db      *sql.DB
	account string
	hash8   string
	model   string
	base    time.Time
	texts   [3]string
	runs    int
}

func newLegacyFixture(t *testing.T) *legacyFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE history (id TEXT PRIMARY KEY, at REAL, account TEXT, plan TEXT, source TEXT, status TEXT, prediction TEXT, path TEXT, detail TEXT)`,
		`CREATE TABLE attempts (identity TEXT PRIMARY KEY, at REAL, pending INTEGER, run_dir TEXT)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	account := "someone@example.com"
	sum := sha256.Sum256([]byte(account))
	hash8 := hex.EncodeToString(sum[:])[:8]
	return &legacyFixture{t: t, dir: dir, db: db, account: account, hash8: hash8, model: "magpie-252-" + hash8 + "/codex/gpt-6-astra",
		base: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), texts: goldenTexts(t, "gpt-6-astra")}
}

func (f *legacyFixture) unix(offset time.Duration) float64 {
	return float64(f.base.Add(offset).UnixNano()) / 1e9
}

// log writes a run log whose closing summary says `prediction`, and returns its path.
func (f *legacyFixture) log(prediction string, texts [3]string) string {
	f.runs++
	path := filepath.Join(f.dir, fmt.Sprintf("run-%d.jsonl", f.runs))
	var lines []string
	add := func(v any) { b, _ := json.Marshal(v); lines = append(lines, string(b)) }
	add(map[string]any{"type": "run_start", "run_id": fmt.Sprint("r", f.runs)})
	for i, text := range texts {
		add(map[string]any{"type": "attempt_terminal", "request_kind": "inference", "outcome": "sample_transport_valid", "detail": map[string]any{"text": text, "expected_count": 300 + i}})
	}
	add(map[string]any{"type": "run_end", "summary": map[string]any{"status": "analysis_received_unverified", "counts": map[string]int{"transport_valid_samples": 3},
		"mt_analysis_raw": map[string]string{"prediction": prediction}, "target": map[string]string{"provider": "codex", "model": "codex/gpt-6-astra"}}})
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// log529 writes a run log that proves a trusted inference 529 (or, with trusted=false, a
// summary that claims it without the final inference terminal record).
func (f *legacyFixture) log529(trusted bool) string {
	f.runs++
	path := filepath.Join(f.dir, fmt.Sprintf("run-%d.jsonl", f.runs))
	d := map[string]any{"http_status": 529}
	lines := []map[string]any{{"type": "run_start"}}
	if trusted {
		lines = append(lines, map[string]any{"type": "attempt_terminal", "request_kind": "inference", "outcome": "unknown", "reason": "http_529", "detail": d})
	}
	lines = append(lines, map[string]any{"type": "run_end", "summary": map[string]any{"status": "unknown", "stop_reason": "inference:unknown:http_529", "stop_detail": d}})
	var out []string
	for _, l := range lines {
		b, _ := json.Marshal(l)
		out = append(out, string(b))
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *legacyFixture) row(offset time.Duration, status, prediction, path, detail string) {
	f.t.Helper()
	f.runs++
	if _, err := f.db.Exec(`INSERT INTO history VALUES (?,?,?,?,?,?,?,?,?)`, fmt.Sprint("id", f.runs), f.unix(offset), f.account, "plus", "252", status, prediction, path, detail); err != nil {
		f.t.Fatal(err)
	}
}

func (f *legacyFixture) match(offset time.Duration) {
	f.row(offset, "match", "gpt-6-astra", f.log("gpt-6-astra", f.texts), "{}")
}

func (f *legacyFixture) attempt(offset time.Duration, pending int) {
	ident, _ := json.Marshal([]string{f.account, "plus"})
	if _, err := f.db.Exec(`INSERT INTO attempts VALUES (?,?,?,?)`, string(ident), f.unix(offset), pending, ""); err != nil {
		f.t.Fatal(err)
	}
}

func (f *legacyFixture) export(models ...string) (TraceSeed, []string, error) {
	f.db.Close()
	if len(models) == 0 {
		models = []string{f.model}
	}
	return ExportLegacySeed(f.dir, models, "", f.base.Add(24*time.Hour))
}

func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		sum := sha256.Sum256(raw)
		info, _ := e.Info()
		out[e.Name()] = hex.EncodeToString(sum[:]) + info.ModTime().String()
	}
	return out
}

func TestLegacyExportDerivesStreakAndAnchorFromDatabaseAndLogs(t *testing.T) {
	f := newLegacyFixture(t)
	f.match(1 * time.Hour)
	f.row(2*time.Hour, "mismatch", "gpt-5.6-sol", f.log("gpt-5.6-sol", goldenTexts(t, "gpt-5.6-sol")), "{}")
	// Streak: three newest matches with a quota row in between, which never interrupts.
	f.match(3 * time.Hour)
	f.row(4*time.Hour, "quota", "", "", "{}")
	f.match(5 * time.Hour)
	f.match(6 * time.Hour)
	f.attempt(6*time.Hour-2*time.Minute, 0)
	f.db.Close()
	before := dirSnapshot(t, f.dir)

	seed, warnings, err := ExportLegacySeed(f.dir, []string{f.model}, "", f.base.Add(24*time.Hour))
	if err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	e := seed.Entries[0]
	if e.Model != f.model || e.Account != "sha256:"+f.hash8 || e.Status != "match" || e.Streak != 3 || e.Expect != "gpt-6-astra" {
		t.Fatalf("%+v", e)
	}
	if !e.AttemptAt.Equal(f.base.Add(6*time.Hour-2*time.Minute)) || !e.LastMatchAt.Equal(f.base.Add(6*time.Hour)) || !e.ResultAt.Equal(e.LastMatchAt) {
		t.Fatalf("times: %+v", e)
	}
	if strings.Contains(fmt.Sprint(seed), "someone@example.com") {
		t.Fatal("the seed leaks the account")
	}
	if after := dirSnapshot(t, f.dir); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the old state directory was modified:\n%v\n%v", before, after)
	}
	var state State
	report, err := ApplyTraceSeed(&state, seed, f.base.Add(7*time.Hour), false)
	if err != nil || report[0].CadenceMinutes != 120 || !report[0].DueAt.Equal(e.AttemptAt.Add(2*time.Hour)) {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestLegacyExportRefusesAmbiguousOrUnsafeState(t *testing.T) {
	t.Run("unknown account hash", func(t *testing.T) {
		f := newLegacyFixture(t)
		f.match(time.Hour)
		f.attempt(time.Hour, 0)
		if _, _, err := f.export("magpie-252-00000000/codex/gpt-6-astra"); err == nil {
			t.Fatal("a model with no matching account was exported")
		}
	})
	t.Run("model id without hash or bank candidate", func(t *testing.T) {
		f := newLegacyFixture(t)
		f.match(time.Hour)
		for _, m := range []string{"gpt-6-astra", "magpie-252-" + f.hash8 + "/codex/not-in-bank"} {
			if _, _, err := f.export(m); err == nil {
				t.Fatal("accepted", m)
			}
			f.db, _ = sql.Open("sqlite", filepath.Join(f.dir, "history.sqlite"))
		}
	})
	t.Run("pending legacy attempt", func(t *testing.T) {
		f := newLegacyFixture(t)
		f.match(time.Hour)
		f.attempt(2*time.Hour, 1)
		if _, _, err := f.export(); err == nil {
			t.Fatal("exported while an old attempt was unresolved")
		}
	})
	t.Run("log contradicts the match row", func(t *testing.T) {
		f := newLegacyFixture(t)
		f.row(time.Hour, "match", "gpt-6-astra", f.log("gpt-5.6-luna", f.texts), "{}")
		f.attempt(time.Hour, 0)
		if _, _, err := f.export(); err == nil {
			t.Fatal("history said match, log said another model")
		}
	})
	t.Run("history prediction is not the expected candidate", func(t *testing.T) {
		f := newLegacyFixture(t)
		f.row(time.Hour, "match", "gpt-5.6-luna", "", "{}")
		f.attempt(time.Hour, 0)
		if _, _, err := f.export(); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestLegacyExportWarnsButDoesNotFailOnMissingLogsAndAttempts(t *testing.T) {
	f := newLegacyFixture(t)
	f.row(time.Hour, "match", "gpt-6-astra", filepath.Join(f.dir, "gone.jsonl"), "{}")
	seed, warnings, err := f.export()
	if err != nil || len(warnings) != 2 {
		t.Fatal(err, warnings)
	}
	if e := seed.Entries[0]; e.Streak != 1 || !e.AttemptAt.Equal(e.ResultAt) {
		t.Fatalf("%+v", e)
	}
}

func TestLegacyExportRescoreDisagreementIsReported(t *testing.T) {
	f := newLegacyFixture(t)
	f.row(time.Hour, "match", "gpt-6-astra", f.log("gpt-6-astra", goldenTexts(t, "gpt-5.6-sol")), "{}")
	f.attempt(time.Hour, 0)
	seed, warnings, err := f.export()
	if err != nil || len(warnings) != 1 || seed.Entries[0].Streak != 1 {
		t.Fatal(err, warnings)
	}
}

func TestLegacyExportOtherNewestStates(t *testing.T) {
	f := newLegacyFixture(t)
	f.match(time.Hour)
	f.row(2*time.Hour, "failed", "", f.log529(true), `{"stop_reason":"inference:unknown:http_529","stop_detail":{"http_status":529}}`)
	f.row(3*time.Hour, "failed", "", f.log529(true), `{"stop_reason":"inference:unknown:http_529","stop_detail":{"http_status":529}}`)
	f.attempt(3*time.Hour, 0)
	seed, _, err := f.export()
	if err != nil {
		t.Fatal(err)
	}
	if e := seed.Entries[0]; e.Status != "degraded" || e.Streak != 0 || e.Streak529 != 2 || !e.At529.Equal(f.base.Add(3*time.Hour)) {
		t.Fatalf("%+v", e)
	}

	g := newLegacyFixture(t)
	g.match(time.Hour)
	g.row(2*time.Hour, "failed", "", "", `{"stop_reason":"inference:unknown:http_400"}`)
	g.attempt(2*time.Hour, 0)
	if seed, _, err = g.export(); err != nil || seed.Entries[0].Status != "unknown" || seed.Entries[0].Streak != 0 {
		t.Fatal(err, seed)
	}

	h := newLegacyFixture(t)
	h.match(time.Hour)
	h.row(2*time.Hour, "mismatch", "gpt-5.6-luna", "", "{}")
	h.attempt(2*time.Hour, 0)
	if seed, _, err = h.export(); err != nil || seed.Entries[0].Status != "mismatch" || seed.Entries[0].Streak != 0 {
		t.Fatal(err, seed)
	}
}

// status.md is only a display: differences warn, they do not decide anything.
func TestLegacyStatusMarkdownIsOnlyADisplayCheck(t *testing.T) {
	f := newLegacyFixture(t)
	f.match(time.Hour)
	f.match(2 * time.Hour)
	f.attempt(2*time.Hour-2*time.Minute, 0)
	f.db.Close()
	jstDue := f.base.Add(2*time.Hour - 2*time.Minute).Add(time.Hour).In(legacyJST).Format("01-02 15:04")
	write := func(streak, interval, due string) string {
		p := filepath.Join(f.dir, "status.md")
		row := fmt.Sprintf("| so***@example.com #%s / plus | match · x | %s | %s | %s |", f.hash8[:4], streak, interval, due)
		os.WriteFile(p, []byte("| 账号 | 最新结果 | 连续一致 | 当前间隔 | 下次可测 |\n| --- | --- | --- | --- | --- |\n"+row+"\n"), 0o600)
		return p
	}
	now := f.base.Add(2*time.Hour + time.Minute)
	if _, w, err := ExportLegacySeed(f.dir, []string{f.model}, write("2", "1 小时", jstDue+" JST"), now); err != nil || len(w) != 0 {
		t.Fatal("matching display produced warnings", err, w)
	}
	if _, w, err := ExportLegacySeed(f.dir, []string{f.model}, write("9", "4 小时", "01-01 00:00 JST"), now); err != nil || len(w) != 3 {
		t.Fatal("differing display should warn three times", err, w)
	}
	if _, w, err := ExportLegacySeed(f.dir, []string{f.model}, filepath.Join(f.dir, "nope.md"), now); err != nil || len(w) != 1 {
		t.Fatal("unreadable display must only warn", err, w)
	}
	if _, w, err := ExportLegacySeed(f.dir, []string{f.model}, write("2", "1 小时", "可测（仍需满足额度门禁）"), now); err != nil || len(w) != 0 {
		t.Fatal("an already-due display row is fine", err, w)
	}
}

func TestLegacyExportUntrusted529IsNotDegraded(t *testing.T) {
	detail := `{"stop_reason":"inference:unknown:http_529","stop_detail":{"http_status":529}}`
	for name, path := range map[string]func(*legacyFixture) string{
		"detail only":       func(*legacyFixture) string { return "" },
		"no final terminal": func(f *legacyFixture) string { return f.log529(false) },
	} {
		f := newLegacyFixture(t)
		f.match(time.Hour)
		f.row(2*time.Hour, "failed", "", path(f), detail)
		f.attempt(2*time.Hour, 0)
		seed, _, err := f.export()
		if err != nil || seed.Entries[0].Status != "unknown" || seed.Entries[0].Streak529 != 0 {
			t.Fatal(name, err, seed)
		}
	}
}
