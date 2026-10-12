package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/mu-zi-lee/magpie-intelligence-check/internal/check"
)

func TestRejectSharedDataDirectory(t *testing.T) {
	dir := t.TempDir()
	lock := flock.New(filepath.Join(dir, "service.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	err := run(dir, filepath.Join(dir, "gate.js"), "127.0.0.1:0", "", false)
	if err == nil || !strings.Contains(err.Error(), "已有检测服务") {
		t.Fatalf("shared directory accepted: %v", err)
	}
}

func TestImportTraceSeedCommandIsDryByDefaultIdempotentAndLocked(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	body := `{"version":1,"source":"test","entries":[{"model":"magpie-252-ca704414/codex/gpt-6-astra","status":"match","streak":8,
	"last_match_at":"2026-10-12T06:49:25Z","result_at":"2026-10-12T06:49:25Z","attempt_at":"2026-10-12T06:47:15Z","expect":"gpt-6-astra","prediction":"gpt-6-astra"}]}`
	if err := os.WriteFile(seed, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	state := func() check.State {
		store, err := check.OpenStore(filepath.Join(data, "check.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var s check.State
		if err = store.Load(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := importTraceSeed([]string{"-data", data, "-seed", seed}); err != nil {
		t.Fatal(err)
	}
	if len(state().TraceSchedule.Runs) != 0 {
		t.Fatal("dry run wrote state")
	}
	if err := importTraceSeed([]string{"-data", data, "-seed", seed, "-apply"}); err != nil {
		t.Fatal(err)
	}
	first := state()
	run := first.TraceSchedule.Runs["magpie-252-ca704414/codex/gpt-6-astra"]
	if run == nil || run.Streak != 8 || first.TraceSchedule.Enabled {
		t.Fatalf("%+v", run)
	}
	if err := importTraceSeed([]string{"-data", data, "-seed", seed, "-apply"}); err != nil {
		t.Fatal(err)
	}
	if second := state(); second.TraceSchedule.Runs["magpie-252-ca704414/codex/gpt-6-astra"].Seed != run.Seed {
		t.Fatal("second import changed the run")
	}
	lock := flock.New(filepath.Join(data, "service.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if err := importTraceSeed([]string{"-data", data, "-seed", seed, "-apply"}); err == nil {
		t.Fatal("imported into a data directory held by a running service")
	}
}

// captureStdout runs fn and returns what it printed to stdout.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = saved
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func TestLegacySnapshotOptInNeverLeavesTheAccountOnStdoutOrInAWorldReadableFile(t *testing.T) {
	if err := exportLegacySeed([]string{"-models", "x/y", "-legacy-snapshot"}); err == nil || !strings.Contains(err.Error(), "-out") {
		t.Fatal("an export with account identifiers must not go to stdout:", err)
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "service.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const account, model = "alice@example.test", "magpie-252-69b1145a/codex/gpt-6-astra"
	body := `{"version":1,"source":"test","entries":[{"model":"` + model + `","account":"sha256:69b1145a","status":"match","streak":8,"expect":"gpt-6-astra","prediction":"gpt-6-astra",
	"last_match_at":"2026-10-12T06:49:25Z","result_at":"2026-10-12T06:49:25Z","attempt_at":"2026-10-12T06:47:15Z"}],
	"legacy_snapshot":{"enabled":true,"state_dir":"` + old + `","history_count":162,"targets":[{"model":"` + model + `","account":"` + account + `","plan":"plus"}]}}`
	seed := filepath.Join(dir, "seed.json")
	data := filepath.Join(dir, "data")
	if err := os.WriteFile(seed, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importTraceSeed([]string{"-data", data, "-seed", seed}); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatal("a world-readable seed with account identifiers was accepted:", err)
	}
	if _, err := os.Stat(data); err == nil {
		t.Fatal("a refused import created the data directory")
	}
	if err := os.Chmod(seed, 0o600); err != nil {
		t.Fatal(err)
	}
	before := mtimes(t, old)
	out, err := captureStdout(t, func() error { return importTraceSeed([]string{"-data", data, "-seed", seed}) })
	if err != nil || strings.Contains(out, account) || !strings.Contains(out, `"legacy_snapshot":{"enabled":true,"targets":1}`) {
		t.Fatal(err, out)
	}
	if out, err = captureStdout(t, func() error { return importTraceSeed([]string{"-data", data, "-seed", seed, "-apply"}) }); err != nil || strings.Contains(out, account) {
		t.Fatal(err, out)
	}
	store, err := check.OpenStore(filepath.Join(data, "check.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state check.State
	if err = store.Load(&state); err != nil {
		t.Fatal(err)
	}
	if state.Compat == nil || state.Compat.Targets[model].Account != account || state.TraceSchedule.Enabled {
		t.Fatal("opt-in not stored privately, or the import enabled the schedule")
	}
	if st, _ := os.Stat(filepath.Join(data, "check.db")); st.Mode().Perm() != 0o600 {
		t.Fatal("state database must be private:", st.Mode())
	}
	if fmt.Sprint(before) != fmt.Sprint(mtimes(t, old)) {
		t.Fatal("importing a seed modified the old state directory")
	}
	// The old collector still running blocks the import.
	running := flock.New(filepath.Join(old, "service.lock"))
	if ok, _ := running.TryLock(); !ok {
		t.Fatal("lock")
	}
	defer running.Unlock()
	if err = importTraceSeed([]string{"-data", filepath.Join(dir, "data2"), "-seed", seed, "-apply"}); err == nil || strings.Contains(err.Error(), account) {
		t.Fatal("imported while the old collector holds its lock, or leaked the account:", err)
	}
}

func mtimes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		out[e.Name()] = info.ModTime().String() + fmt.Sprint(info.Size())
	}
	return out
}
