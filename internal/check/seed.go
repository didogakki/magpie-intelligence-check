package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A trace seed carries the facts of an earlier collector's run history for a model, so
// the schedule continues instead of restarting at the 15 minute step. It holds facts
// (streak, when the newest match and the newest attempt happened), never due times:
// the scheduler derives those with the same cadence rules as for native runs.
//
// Importing is explicit and idempotent. It changes only the run state of the listed
// models: it neither enables the schedule nor creates targets, and it never overwrites
// a run that already has a newer attempt than the seed.
const (
	traceSeedVersion = 1
	seedStatusMatch  = "match"
	seedStatusMiss   = "mismatch"
	seedStatus529    = "degraded"
	seedStatusOther  = "unknown"
)

type TraceSeed struct {
	Version    int              `json:"version"`
	Source     string           `json:"source"`
	ExportedAt time.Time        `json:"exported_at"`
	Entries    []TraceSeedEntry `json:"entries"`
	// LegacySnapshot is the explicit opt-in for the compatibility snapshot writer (compat.go).
	// It carries the old account identifiers, so a seed that has it is a private file.
	LegacySnapshot *LegacySnapshotSeed `json:"legacy_snapshot,omitempty"`
}

type TraceSeedEntry struct {
	Model       string    `json:"model"`            // model ID of this plugin's target
	Account     string    `json:"account"`          // audit reference only, e.g. "sha256:ca704414"
	Expect      string    `json:"expect,omitempty"` // bank candidate that counted as a match
	Status      string    `json:"status"`           // result of the newest scored/failed attempt
	Prediction  string    `json:"prediction,omitempty"`
	Streak      int       `json:"streak"`
	LastMatchAt time.Time `json:"last_match_at,omitzero"`
	ResultAt    time.Time `json:"result_at"`  // when the newest result was recorded
	AttemptAt   time.Time `json:"attempt_at"` // when the newest attempt started: the cadence anchor
	Streak529   int       `json:"streak_529,omitempty"`
	At529       time.Time `json:"at_529,omitzero"`
	// The newest scored (match/mismatch) result when Status is not one itself, so a
	// fresh result that a later failure sits on top of is not lost.
	ScoredAt         time.Time `json:"scored_at,omitzero"`
	ScoredPrediction string    `json:"scored_prediction,omitempty"`
}

// Digest identifies the entry's content; importing the same content again is a no-op.
func (e TraceSeedEntry) Digest() string {
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func (e TraceSeedEntry) validate() error {
	switch {
	case e.Model == "" || strings.ContainsAny(e.Model, "\r\n"):
		return errors.New("种子缺少模型 ID")
	case e.AttemptAt.IsZero() || e.ResultAt.IsZero():
		return fmt.Errorf("%s：种子缺少时间", e.Model)
	case e.Streak < 0 || e.Streak529 < 0:
		return fmt.Errorf("%s：种子计数无效", e.Model)
	}
	if !e.ScoredAt.IsZero() || e.ScoredPrediction != "" {
		if e.Status == seedStatusMatch || e.Status == seedStatusMiss || e.ScoredAt.IsZero() || e.ScoredPrediction == "" || e.Expect == "" || e.ScoredAt.After(e.ResultAt) {
			return fmt.Errorf("%s：种子的最近评分结果无效", e.Model)
		}
	}
	switch e.Status {
	case seedStatusMatch:
		if e.Streak < 1 || e.LastMatchAt.IsZero() {
			return fmt.Errorf("%s：match 种子需要连续次数和最近一致时间", e.Model)
		}
	case seedStatusMiss, seedStatusOther:
		if e.Streak != 0 {
			return fmt.Errorf("%s：非 match 种子的连续次数必须为 0", e.Model)
		}
	case seedStatus529:
		if e.Streak != 0 || e.Streak529 < 1 || e.At529.IsZero() {
			return fmt.Errorf("%s：529 种子需要 529 次数和时间", e.Model)
		}
	default:
		return fmt.Errorf("%s：未知种子状态 %q", e.Model, e.Status)
	}
	return nil
}

// SeedImport is the outcome for one entry.
type SeedImport struct {
	Model          string    `json:"model"`
	Action         string    `json:"action"` // imported | would_import | already_imported | skipped_newer_state
	Streak         int       `json:"streak"`
	CadenceMinutes int       `json:"cadence_minutes"`
	DueAt          time.Time `json:"due_at"`
}

// ApplyTraceSeed merges seed into state at time now. With apply false it only reports
// what would change. Nothing is modified when any entry is invalid.
func ApplyTraceSeed(state *State, seed TraceSeed, now time.Time, apply bool) ([]SeedImport, error) {
	return applyTraceSeed(state, seed, now, apply, "")
}

// applyTraceSeed is ApplyTraceSeed for a running service, which may already hold the old
// collector's lock for heldDir.
func applyTraceSeed(state *State, seed TraceSeed, now time.Time, apply bool, heldDir string) ([]SeedImport, error) {
	if seed.Version != traceSeedVersion {
		return nil, fmt.Errorf("不支持的种子版本 %d", seed.Version)
	}
	if len(seed.Entries) == 0 || len(seed.Entries) > maxScheduledModels {
		return nil, errors.New("种子条目数量无效")
	}
	seen := map[string]bool{}
	for _, e := range seed.Entries {
		if err := e.validate(); err != nil {
			return nil, err
		}
		if seen[e.Model] {
			return nil, fmt.Errorf("%s：种子中重复", e.Model)
		}
		seen[e.Model] = true
	}
	var compat *LegacyCompat
	if seed.LegacySnapshot != nil {
		var err error
		if compat, err = checkLegacySnapshotSeed(seed.LegacySnapshot, seed.Entries, now, heldDir); err != nil {
			return nil, err
		}
	}
	sch := &state.TraceSchedule
	if apply && sch.Runs == nil {
		sch.Runs = map[string]*TraceRun{}
	}
	var report []SeedImport
	for _, e := range seed.Entries {
		digest := e.Digest()
		run := sch.Runs[e.Model]
		switch {
		case run != nil && run.Seed == digest:
			report = append(report, seedReport(e, "already_imported", run, now))
			continue
		case run != nil && !run.Attempt.Before(e.AttemptAt):
			report = append(report, seedReport(e, "skipped_newer_state", run, now))
			continue
		}
		next := seedRun(e, digest, run)
		action := "would_import"
		if apply {
			action = "imported"
			sch.Runs[e.Model] = next
		}
		report = append(report, seedReport(e, action, next, now))
	}
	if apply && seed.LegacySnapshot != nil {
		if compat != nil && state.Compat != nil && state.Compat.StateDir == compat.StateDir && state.Compat.HistoryBase == compat.HistoryBase {
			compat.HistoryAdded = state.Compat.HistoryAdded // the same import again keeps the running count
		}
		state.Compat = compat
	}
	return report, nil
}

// seedRun builds the run state for an entry, keeping the daily counters of an existing run.
func seedRun(e TraceSeedEntry, digest string, existing *TraceRun) *TraceRun {
	run := &TraceRun{
		Attempt: e.AttemptAt, Period: e.AttemptAt, At: e.ResultAt, Outcome: "imported", Class: seedClass(e.Status),
		Classified: "seed:" + digest, Streak: e.Streak, LastMatchAt: e.LastMatchAt, Seed: digest,
		Detail: "已从旧 ModelTrace 采集器导入连续一致次数和最近一次开始时间",
	}
	run.ClassAt = e.ResultAt
	if e.Prediction != "" && e.Expect != "" {
		run.Fingerprint = &TraceFingerprint{Expect: e.Expect, Prediction: e.Prediction, At: e.ResultAt}
	} else if e.ScoredPrediction != "" {
		run.Fingerprint = &TraceFingerprint{Expect: e.Expect, Prediction: e.ScoredPrediction, At: e.ScoredAt}
	}
	run.Paused = e.Status == seedStatusOther
	if e.Status == seedStatus529 {
		run.Streak529, run.At529 = e.Streak529, e.At529
		run.NextDue = traceNextDue(run, e.ResultAt)
	}
	if existing != nil {
		run.Day, run.DayRuns = existing.Day, existing.DayRuns
	}
	return run
}

func seedClass(status string) string {
	switch status {
	case seedStatusMatch:
		return classMatch
	case seedStatusMiss:
		return classMismatch
	case seedStatus529:
		return class529
	}
	return classUnknown
}

func seedReport(e TraceSeedEntry, action string, run *TraceRun, now time.Time) SeedImport {
	return SeedImport{Model: e.Model, Action: action, Streak: run.Streak, CadenceMinutes: int(traceCadence(traceStreakAt(run, now)) / time.Minute), DueAt: traceDueAt(run, now)}
}

// ImportTraceSeed applies a seed to the running service and saves it. It does not enable
// the schedule and cannot start a request by itself.
func (s *Service) ImportTraceSeed(seed TraceSeed, apply bool) ([]SeedImport, error) {
	held := s.heldLegacyDir() // before s.mu: the compat writer takes its own lock first
	s.mu.Lock()
	defer s.mu.Unlock()
	if !apply {
		return applyTraceSeed(&s.state, seed, s.traceNow(), false, held) // a dry run only reads
	}
	previous := make(map[string]*TraceRun, len(s.state.TraceSchedule.Runs))
	for k, v := range s.state.TraceSchedule.Runs {
		c := *v
		previous[k] = &c
	}
	previousCompat := s.state.Compat
	report, err := applyTraceSeed(&s.state, seed, s.traceNow(), true, held)
	if err != nil {
		return nil, err
	}
	if err = s.store.Save(s.state); err != nil {
		s.state.TraceSchedule.Runs = previous
		s.state.Compat = previousCompat
		return nil, err
	}
	s.wakeScheduler()
	return report, nil
}
