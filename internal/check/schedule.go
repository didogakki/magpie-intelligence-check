package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"time"
)

// Cadence of the old magpie-modeltrace/service.py, per model: after n consecutive
// matching batches the next batch is due 15·2^min(n,4) minutes after the previous
// attempt started (15/30/60/120/240 min). Any other result sets n back to 0, and a
// streak whose newest match is older than the stale threshold counts as 0 again.
const (
	traceBaseline  = 15 * time.Minute
	traceMaxStep   = 4
	traceStreakTTL = 6 * time.Hour

	defaultTraceInterval = 15 // minutes; the baseline, and the only accepted interval_minutes
	defaultTraceDailyCap = 8  // scheduled runs per model per UTC day (4h cadence needs 6)
	maxTraceDailyCap     = 24
	maxScheduledModels   = 32
	traceRequests        = 3
	// Scheduled runs may use at most 1/scheduleBudgetShare of a model's daily
	// request budget; the rest stays available to the milk gate and manual tests.
	scheduleBudgetShare = 2
	// Upper bound for one sleep, so a laptop suspend cannot delay a tick for hours.
	scheduleWakeCap = 10 * time.Minute
	// Back-off after a skipped or failed start, so a blocked model never spins the loop.
	scheduleSkipRetry = 5 * time.Minute
)

// Backoff after a classified batch, per model. A typed HTTP 529 waits 1/2/4/6h from the
// batch, a confirmed network/timeout/5xx failure 3/6/12m; the last step repeats.
var (
	trace529Waits       = []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour}
	traceTransientWaits = []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute}
)

// Batch classes. Only a scored batch (match or mismatch) clears failures.
const (
	classMatch     = "match"     // three usable answers whose closest bank candidate is the expected model
	classMismatch  = "mismatch"  // three usable answers whose closest candidate is another model
	class529       = "529"       // a probe got a real HTTP 529
	classTransient = "transient" // a probe hit a confirmed network error, timeout or HTTP 5xx
	classUnknown   = "unknown"   // anything else: refusal, cut-off, short answer, 4xx, bare HTTP 200
)

// TraceModelPlan is the per-model schedule. A model is scheduled only if it is listed.
// IntervalMinutes is kept for stored/API compatibility; it is always the 15 minute baseline
// and the real spacing follows the streak. Expect names the bank candidate that counts as
// a match; empty means the last path segment of the model ID.
type TraceModelPlan struct {
	IntervalMinutes int    `json:"interval_minutes"`
	DailyCap        int    `json:"daily_cap"`
	Expect          string `json:"expect,omitempty"`
}

// TraceFingerprint is the last scored batch of a model. It is a closed-set fingerprint
// result over the answers of one channel, not proof of the physical model or account.
type TraceFingerprint struct {
	Expect      string    `json:"expect"`
	Prediction  string    `json:"prediction"`
	Probability float64   `json:"probability,omitempty"`
	At          time.Time `json:"at"`
}

// TraceRun is the durable bookkeeping of the schedule of one model. Attempt is written
// before any request is sent, so a restart can never repeat the attempt.
type TraceRun struct {
	Period  time.Time `json:"period"`              // due instant that the latest reservation answered
	Attempt time.Time `json:"attempt_at,omitzero"` // start of the latest batch: anchor of the cadence
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
	Batch   string    `json:"batch,omitempty"`
	At      time.Time `json:"at"`
	Day     string    `json:"day,omitempty"`
	DayRuns int       `json:"day_runs,omitempty"`

	// Result state, per model. All fields are optional, so records stored before the
	// cadence existed load unchanged.
	Class       string            `json:"class,omitempty"`        // class of the last classified batch
	Classified  string            `json:"classified,omitempty"`   // last classified batch; a batch is never classified twice
	ClassAt     time.Time         `json:"class_at,omitzero"`      // when that batch was classified
	Streak      int               `json:"streak,omitempty"`       // consecutive matching batches
	LastMatchAt time.Time         `json:"last_match_at,omitzero"` // when the newest match was recorded
	Fingerprint *TraceFingerprint `json:"fingerprint,omitempty"`
	Streak529   int               `json:"streak_529,omitempty"` // consecutive 529 batches; only a scored batch ends it
	At529       time.Time         `json:"at_529,omitzero"`      // when the latest 529 batch finished
	Transient   int               `json:"transient,omitempty"`  // consecutive confirmed transient batches
	NextDue     time.Time         `json:"next_due,omitzero"`    // cooldown deadline of a 529 / transient failure
	Retry       time.Time         `json:"retry_at,omitzero"`    // back-off after a skipped or failed start
	Seed        string            `json:"seed,omitempty"`       // digest of the imported legacy state this run started from
	Paused      bool              `json:"paused,omitempty"`     // unconfirmed outcome: no automatic batch until a conclusive batch resolves it
}

type TraceSchedule struct {
	Enabled bool                      `json:"enabled"`
	Models  map[string]TraceModelPlan `json:"models"`
	Runs    map[string]*TraceRun      `json:"runs,omitempty"`
}

// TraceScheduleStatus is the read-only view shown by the console.
type TraceScheduleStatus struct {
	Model           string    `json:"model"`
	Scheduled       bool      `json:"scheduled"`
	IntervalMinutes int       `json:"interval_minutes"`
	DailyCap        int       `json:"daily_cap"`
	TodayRuns       int       `json:"today_runs"`
	Outcome         string    `json:"outcome,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	Class           string    `json:"class,omitempty"`
	Streak          int       `json:"streak"`
	CadenceMinutes  int       `json:"cadence_minutes"`
	Streak529       int       `json:"streak_529,omitempty"`
	Transient       int       `json:"transient,omitempty"`
	Expect          string    `json:"expect,omitempty"`
	Prediction      string    `json:"prediction,omitempty"`
	Probability     float64   `json:"probability,omitempty"`
	NextDue         time.Time `json:"next_due,omitzero"`
	RunState        string    `json:"run_state,omitempty"`
	Batch           string    `json:"batch,omitempty"`
	LastAt          time.Time `json:"last_at,omitzero"`
	NextAt          time.Time `json:"next_at,omitzero"`
	Paused          bool      `json:"paused,omitempty"`
	PauseReason     string    `json:"pause_reason,omitempty"`
	Imported        bool      `json:"imported,omitempty"`
	Note            string    `json:"note"`
}

// tracePauseReason is shown instead of a next-due time while a model is paused. The old
// collector's schedule() returned pending_unresolved for such a batch and never retried it.
const tracePauseReason = "上一轮结果未确认（拒答、截断、回答不足、4xx、仅 HTTP 200 或批次不完整），已暂停自动检测；需手动运行一轮完整检测并得出明确结果后才会恢复"

const traceFingerprintNote = "指纹结果：与候选库中最相似模型的比较，不能证明物理身份或账号"

func (s *Service) normalizeScheduleLocked() {
	sch := &s.state.TraceSchedule
	if sch.Models == nil {
		sch.Models = map[string]TraceModelPlan{}
	}
	if sch.Runs == nil {
		sch.Runs = map[string]*TraceRun{}
	}
	// The spacing follows the streak; the stored interval is only the baseline.
	for model, plan := range sch.Models {
		plan.IntervalMinutes = defaultTraceInterval
		sch.Models[model] = plan
	}
	for _, run := range sch.Runs {
		if run.Outcome == "reserved" {
			run.Outcome = "interrupted"
			run.Detail = "检测服务重启，本次不重试；按上次开始时间继续计划"
		}
	}
}

// SetTraceSchedule replaces the schedule configuration. Last results and daily
// counts are kept. It never touches targets, so the milk gate file is not rewritten.
func (s *Service) SetTraceSchedule(enabled bool, models map[string]TraceModelPlan) error {
	return s.setTraceScheduleAt(enabled, models, s.traceNow())
}

// setTraceScheduleAt is SetTraceSchedule with an injectable clock. A model that
// becomes newly eligible and has never been attempted is anchored at `now`, persisted
// before the scheduler is woken, so its first run is one baseline later even after a
// restart. A model with existing (or imported) state keeps it and is simply due when
// that state says so.
func (s *Service) setTraceScheduleAt(enabled bool, models map[string]TraceModelPlan, now time.Time) error {
	if len(models) > maxScheduledModels {
		return fmt.Errorf("定时检测最多选择 %d 个模型", maxScheduledModels)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]TraceModelPlan, len(models))
	for model, plan := range models {
		if s.state.Targets[model] == nil {
			return errors.New("模型未配置，请先添加监测目标：" + model)
		}
		if plan.IntervalMinutes == 0 {
			plan.IntervalMinutes = defaultTraceInterval
		}
		if plan.DailyCap == 0 {
			plan.DailyCap = defaultTraceDailyCap
		}
		if plan.IntervalMinutes != defaultTraceInterval {
			return fmt.Errorf("基线间隔固定为 %d 分钟，实际间隔随连续一致次数变化", defaultTraceInterval)
		}
		if plan.DailyCap < 1 || plan.DailyCap > maxTraceDailyCap {
			return fmt.Errorf("每日定时次数须为 1–%d 次", maxTraceDailyCap)
		}
		if fingerprintModelFor(model, plan.Expect) == "" {
			return errors.New("指纹库中没有可与之比较的候选，请在 expect 中指定候选模型：" + model)
		}
		next[model] = plan
	}
	sch := &s.state.TraceSchedule
	previous := *sch
	previousRuns := make(map[string]*TraceRun, len(sch.Runs))
	for model, run := range sch.Runs {
		c := *run
		previousRuns[model] = &c
	}
	if sch.Runs == nil {
		sch.Runs = map[string]*TraceRun{}
	}
	for model := range next {
		if _, had := previous.Models[model]; previous.Enabled && had {
			continue
		}
		if run := sch.Runs[model]; run == nil {
			sch.Runs[model] = &TraceRun{Attempt: now}
		} else if run.Attempt.IsZero() {
			run.Attempt = now
		}
	}
	sch.Enabled, sch.Models = enabled, next
	if err := s.store.Save(s.state); err != nil {
		s.state.TraceSchedule = previous
		s.state.TraceSchedule.Runs = previousRuns
		return err
	}
	s.wakeScheduler()
	return nil
}

func (s *Service) wakeScheduler() {
	select {
	case s.schedWake <- struct{}{}:
	default:
	}
	s.wakeCompat()
}

func (s *Service) traceNow() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// traceCadence is the spacing after `streak` consecutive matches.
func traceCadence(streak int) time.Duration {
	return traceBaseline << min(max(streak, 0), traceMaxStep)
}

// traceStreakAt is the streak that sets the spacing at `now`: a streak whose newest
// match is older than the stale threshold no longer counts.
func traceStreakAt(run *TraceRun, now time.Time) int {
	if run == nil || run.Streak <= 0 || run.LastMatchAt.IsZero() || now.Sub(run.LastMatchAt) > traceStreakTTL {
		return 0
	}
	return run.Streak
}

// traceDueAt is when the model's next scheduled batch may start. A 529 or transient
// batch is governed by its cooldown alone (the cooldown replaces the baseline); every
// other state waits one cadence step after the last attempt, and still honours an
// unexpired 529 deadline. A zero result means the model has never been anchored.
func traceDueAt(run *TraceRun, now time.Time) time.Time {
	if run == nil || run.Attempt.IsZero() {
		return time.Time{}
	}
	due := run.Attempt.Add(traceCadence(traceStreakAt(run, now)))
	switch run.Class {
	case class529, classTransient:
		if !run.NextDue.IsZero() {
			due = run.NextDue
		}
	default:
		due = later(due, run.NextDue)
	}
	return later(due, run.Retry)
}

// traceFailure extracts the typed facts of a failed probe: the real HTTP status, or a
// confirmed transport failure. Error text is never inspected.
func traceFailure(err error) (status int, kind string) {
	var he *HTTPStatusError
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &he):
		return he.Status, ""
	case errors.Is(err, context.Canceled):
		return 0, ""
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return 0, "timeout"
	case errors.As(err, &op), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return 0, "network"
	}
	return 0, ""
}

// classifyTraceBatch classifies one whole three-result batch. A real HTTP 529 wins; then
// a confirmed transient failure; otherwise the three answers must all be usable for the
// fingerprint, and the closest bank candidate decides match or mismatch. Anything else,
// including a bare HTTP 200, a refusal or a cut-off answer, is unknown. The report is
// non-nil only for a scored batch.
func classifyTraceBatch(results []Result, expect string) (string, *FingerprintReport) {
	if len(results) != traceRequests {
		return classUnknown, nil
	}
	transient := false
	for _, r := range results {
		if r.HTTPStatus == 529 {
			return class529, nil
		}
		transient = transient || r.ErrorKind == "timeout" || r.ErrorKind == "network" || r.HTTPStatus >= 500 && r.HTTPStatus < 600
	}
	if transient {
		return classTransient, nil
	}
	outputs := make([]FingerprintOutput, len(results))
	for i, r := range results {
		if r.Verdict != "manual" || r.Error != "" || r.ExpectedCount <= 0 {
			return classUnknown, nil
		}
		outputs[i] = FingerprintOutput{Text: r.Answer, ExpectedCount: r.ExpectedCount}
	}
	if expect == "" {
		return classUnknown, nil
	}
	report, err := AnalyzeFingerprint(outputs)
	if err != nil || report.UsedOutputs != traceRequests {
		return classUnknown, nil
	}
	if report.Prediction == expect {
		return classMatch, &report
	}
	return classMismatch, &report
}

// traceNextDue is the later of the 529 deadline and the transient cooldown; zero means
// no cooldown applies. A 529 marker has no TTL: only a scored batch ends it.
func traceNextDue(run *TraceRun, now time.Time) time.Time {
	var due time.Time
	if run.Streak529 > 0 {
		due = run.At529.Add(trace529Waits[min(run.Streak529, len(trace529Waits))-1])
	}
	if run.Transient > 0 {
		due = later(due, now.Add(traceTransientWaits[min(run.Transient, len(traceTransientWaits))-1]))
	}
	return due
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// classifyTraceBatchLocked classifies a finished batch once (manual batches too) and
// persists the streaks and cooldown with it. Any result but a match ends the match
// streak; unknown ends a transient streak but never clears an active 529.
func (s *Service) classifyTraceBatchLocked(model, batch string, results []Result, now time.Time) {
	sch := &s.state.TraceSchedule
	expect := fingerprintModelFor(model, sch.Models[model].Expect)
	class, report := classifyTraceBatch(results, expect)
	run := sch.Runs[model]
	if run == nil {
		run = &TraceRun{}
		if sch.Runs == nil {
			sch.Runs = map[string]*TraceRun{}
		}
		sch.Runs[model] = run
	}
	if run.Classified == batch {
		return
	}
	run.Classified, run.Class, run.ClassAt = batch, class, now
	if c := s.state.Compat; c != nil {
		if _, ok := c.Targets[model]; ok {
			c.HistoryAdded++ // one old-collector history row per classified batch
		}
	}
	// A manual batch is an attempt too: the cadence counts from the newest start.
	run.Attempt = later(run.Attempt, results[0].Time)
	switch class {
	case classMatch, classMismatch:
		run.Streak529, run.At529, run.Transient = 0, time.Time{}, 0
		run.Fingerprint = &TraceFingerprint{Expect: expect, Prediction: report.Prediction, Probability: report.Probability, At: now}
		if class == classMatch {
			run.Streak++
			run.LastMatchAt = now
		} else {
			run.Streak = 0
		}
	case class529:
		run.Streak = 0
		run.Streak529++
		run.At529, run.Transient = now, 0
	case classTransient:
		run.Streak = 0
		run.Transient++
	default:
		run.Streak, run.Transient = 0, 0
	}
	// An unconfirmed batch pauses the model; any conclusive batch (even a manual one) resumes it.
	run.Paused = class == classUnknown
	run.NextDue = traceNextDue(run, now)
	run.Retry = time.Time{}
	s.wakeCompat()
}

func (s *Service) traceUsageLocked(model string, day string) int {
	count := s.state.ClearedUsage[day][model]
	for _, r := range s.state.Results {
		if r.Model == model && r.Time.UTC().Format("2006-01-02") == day {
			count++
		}
	}
	return count
}

func nextUTCDay(now time.Time) time.Duration {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 1, 0, time.UTC).Sub(now)
}

// scheduleSkipLocked returns why a due batch must not start now ("" means go), and how
// long to leave the model alone before looking again.
func (s *Service) scheduleSkipLocked(model string, plan TraceModelPlan, run *TraceRun, now time.Time) (reason, detail string, retry time.Duration) {
	day := now.UTC().Format("2006-01-02")
	t := s.state.Targets[model]
	switch {
	case t == nil:
		return "target_missing", "监测目标已删除", scheduleSkipRetry
	case !t.Enabled:
		return "target_disabled", "监测目标已关闭", scheduleSkipRetry
	case s.state.Config.GatewayURL == "" || s.state.Config.GatewayKey == "":
		return "gateway_unconfigured", "尚未配置 Magpie 网关", scheduleSkipRetry
	case s.running[model]:
		return "running", "该模型已有检测进行中，稍后再看", time.Minute
	case run.Day == day && run.DayRuns >= plan.DailyCap:
		return "daily_cap", fmt.Sprintf("今日已定时检测 %d 次，达到上限", run.DayRuns), nextUTCDay(now)
	case s.traceUsageLocked(model, day)+traceRequests > s.state.Config.DailyLimit/scheduleBudgetShare:
		return "budget", "今日预算不足；定时检测最多使用每日预算的一半", nextUTCDay(now)
	}
	return "", "", 0
}

// scheduleModel handles one model at `now` and returns when to look at it next.
func (s *Service) scheduleModel(model string, now time.Time) time.Time {
	s.mu.Lock()
	sch := &s.state.TraceSchedule
	plan, ok := sch.Models[model]
	if !sch.Enabled || !ok {
		s.mu.Unlock()
		return now.Add(scheduleWakeCap)
	}
	run := sch.Runs[model]
	if run == nil || run.Attempt.IsZero() {
		// Never anchored (state written by hand): start the clock instead of testing at once.
		var previous *TraceRun
		if run == nil {
			run = &TraceRun{}
			sch.Runs[model] = run
		} else {
			c := *run
			previous = &c
		}
		run.Attempt = now
		if err := s.store.Save(s.state); err != nil {
			if previous == nil {
				delete(sch.Runs, model)
			} else {
				*run = *previous
			}
			s.health = err.Error()
		}
		s.mu.Unlock()
		return now.Add(traceBaseline)
	}
	if run.Paused {
		s.mu.Unlock()
		return now.Add(scheduleWakeCap)
	}
	due := traceDueAt(run, now)
	if now.Before(due) {
		s.mu.Unlock()
		return due
	}
	previous := *run
	fail := func(err error) {
		*run = previous
		s.health = err.Error()
	}
	if reason, detail, retry := s.scheduleSkipLocked(model, plan, run, now); reason != "" {
		run.Outcome, run.Detail, run.Batch, run.At, run.Retry = reason, detail, "", now, now.Add(retry)
		if err := s.store.Save(s.state); err != nil {
			fail(err)
			s.mu.Unlock()
			return now.Add(scheduleSkipRetry)
		}
		s.mu.Unlock()
		return run.Retry
	}
	// Persist the reservation first: no durable reservation, no request.
	day := now.UTC().Format("2006-01-02")
	if run.Day != day {
		run.Day, run.DayRuns = day, 0
	}
	run.DayRuns++
	run.Period, run.Attempt, run.Retry = due, now, time.Time{}
	run.Outcome, run.Detail, run.Batch, run.At = "reserved", "", "", now
	if err := s.store.Save(s.state); err != nil {
		fail(err)
		s.mu.Unlock()
		return now.Add(scheduleSkipRetry)
	}
	s.mu.Unlock()

	batch, err := s.triggerModelTraceAt(model, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if run = sch.Runs[model]; run == nil || !run.Period.Equal(due) || !run.Attempt.Equal(now) {
		return now.Add(scheduleSkipRetry)
	}
	if err != nil {
		// Nothing was sent: give the attempt back and look again shortly.
		run.Outcome, run.Detail, run.DayRuns = "trigger_error", err.Error(), max(run.DayRuns-1, 0)
		run.Attempt, run.Retry = previous.Attempt, now.Add(scheduleSkipRetry)
	} else {
		run.Outcome, run.Batch = "started", batch
	}
	if e := s.store.Save(s.state); e != nil {
		s.health = e.Error()
	}
	return later(traceDueAt(run, now), now.Add(time.Second))
}

// scheduleTick processes every scheduled model and returns how long to sleep.
// It returns 0 when nothing is scheduled: the goroutine then waits for a config change.
func (s *Service) scheduleTick(now time.Time) time.Duration {
	s.mu.Lock()
	sch := s.state.TraceSchedule
	models := make([]string, 0, len(sch.Models))
	if sch.Enabled {
		for model := range sch.Models {
			models = append(models, model)
		}
	}
	s.mu.Unlock()
	if len(models) == 0 {
		return 0
	}
	sort.Strings(models)
	wait := scheduleWakeCap
	for _, model := range models {
		if d := s.scheduleModel(model, now).Sub(now); d < wait {
			wait = d
		}
	}
	return max(wait, time.Second)
}

// StartTraceSchedule runs the single scheduler goroutine. It is idle, with no
// timer, while the schedule is disabled or empty.
func (s *Service) StartTraceSchedule() {
	s.schedOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				var timer *time.Timer
				var fire <-chan time.Time
				if wait := s.scheduleTick(s.traceNow()); wait > 0 {
					timer = time.NewTimer(wait)
					fire = timer.C
				}
				select {
				case <-s.ctx.Done():
					if timer != nil {
						timer.Stop()
					}
					return
				case <-s.schedWake:
				case <-fire:
				}
				if timer != nil {
					timer.Stop()
				}
			}
		}()
		s.startLegacyCompat()
	})
}

func (s *Service) traceRunStateLocked(batch string) string {
	if batch == "" {
		return ""
	}
	found, manual := 0, 0
	for _, r := range s.state.Results {
		if r.Prompt != "modeltrace" || r.Batch != batch {
			continue
		}
		found++
		switch r.Verdict {
		case "running":
			return "running"
		case "manual":
			manual++
		}
	}
	switch {
	case found == 0:
		return "records_cleared"
	case manual == found:
		return "completed"
	}
	return "unavailable"
}

// TraceScheduleStatus lists every scheduled model plus any target with leftover run history.
func (s *Service) TraceScheduleStatus(now time.Time) []TraceScheduleStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch := s.state.TraceSchedule
	names := map[string]bool{}
	for model := range sch.Models {
		names[model] = true
	}
	for model := range sch.Runs {
		names[model] = true
	}
	out := make([]TraceScheduleStatus, 0, len(names))
	day := now.UTC().Format("2006-01-02")
	for model := range names {
		plan, scheduled := sch.Models[model]
		st := TraceScheduleStatus{Model: model, Scheduled: scheduled, IntervalMinutes: plan.IntervalMinutes, DailyCap: plan.DailyCap,
			Expect: fingerprintModelFor(model, plan.Expect), CadenceMinutes: int(traceBaseline / time.Minute), Note: traceFingerprintNote}
		run := sch.Runs[model]
		if run != nil {
			st.Outcome, st.Detail, st.Batch, st.LastAt = run.Outcome, run.Detail, run.Batch, run.At
			st.Class, st.Streak529, st.Transient, st.NextDue = run.Class, run.Streak529, run.Transient, run.NextDue
			st.Streak = run.Streak
			st.CadenceMinutes = int(traceCadence(traceStreakAt(run, now)) / time.Minute)
			st.Imported = run.Seed != ""
			if run.Fingerprint != nil {
				st.Prediction, st.Probability = run.Fingerprint.Prediction, run.Fingerprint.Probability
			}
			if run.Day == day {
				st.TodayRuns = run.DayRuns
			}
			if run.Outcome == "started" {
				st.RunState = s.traceRunStateLocked(run.Batch)
			}
		}
		if run != nil && run.Paused {
			st.Paused, st.PauseReason = true, tracePauseReason
			st.NextDue = time.Time{}
		} else if sch.Enabled && scheduled {
			st.NextAt = traceDueAt(run, now)
			if st.NextAt.IsZero() {
				st.NextAt = now.Add(traceBaseline)
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}
