package check

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Compatibility snapshot writer for the old widget reader.
//
// /Users/…/magpie-widget/proxy.py serves /api/records from <old state dir>/snapshot.json,
// which the old magpie-modeltrace/service.py rewrote after every attempt. Once that
// collector is stopped nothing refreshes the file, and its records expire after 6 hours.
// This writer keeps the same file (schema version 1, byte-compatible field set) current
// from the results of the new plugin's model targets, and nothing else.
//
// It writes only when ALL of these hold, and otherwise never touches the old directory:
//   - a seed import carrying "legacy_snapshot": {"enabled": true, ...} was applied (explicit opt-in);
//   - the trace schedule is enabled, and every mapped model is scheduled with its target enabled;
//   - this process holds the old collector's service.lock, i.e. the old collector is not running
//     and cannot be started again while the writer is active;
//   - the existing snapshot.json, if any, is a regular version-1 file whose live records are all
//     covered by the mapping (nothing is silently dropped).
//
// The snapshot carries the account identifier the old widget keys on, plus plan, model,
// result times and the degraded fields. It never carries prompts, answers or credentials.
const (
	compatProvider          = "codex" // pinned to what the old collector wrote
	compatSource            = "252"
	compatSnapshotVersion   = 1
	compatDegradedExpiresAt = 253402300799 // 9999-12-31T23:59:59Z: the old collector's finite stand-in for "no TTL"
	compatSnapshotFile      = "snapshot.json"
	compatStatusFile        = "status.md"
	compatLockFile          = "service.lock"
	compatTick              = time.Minute
	compatHeartbeat         = 5 * time.Minute
	compatMaxFile           = 1 << 20
)

// LegacySnapshotSeed is the seed's opt-in section. Enabled=false (with nothing else) switches
// the writer off again. Targets map each seeded model to exactly one old (account, plan).
type LegacySnapshotSeed struct {
	Enabled      bool                   `json:"enabled"`
	StateDir     string                 `json:"state_dir,omitempty"`     // old collector state dir: snapshot.json, status.md, service.lock
	HistoryCount int                    `json:"history_count,omitempty"` // rows in the old history table at export
	Targets      []LegacySnapshotTarget `json:"targets,omitempty"`
}

type LegacySnapshotTarget struct {
	Model   string `json:"model"`
	Account string `json:"account"`
	Plan    string `json:"plan"`
}

// LegacyCompat is the stored, private form (inside the 0600 state database). The console
// never receives it: Service.Snapshot drops it.
type LegacyCompat struct {
	StateDir     string                        `json:"state_dir"`
	HistoryBase  int                           `json:"history_base"`
	HistoryAdded int                           `json:"history_added"` // batches classified since the import
	Targets      map[string]LegacyCompatTarget `json:"targets"`
}

type LegacyCompatTarget struct {
	Account string `json:"account"`
	Plan    string `json:"plan"`
	Expect  string `json:"expect"` // the snapshot's "model": the bank candidate that counts as a match
}

var errLegacyRunning = errors.New("旧采集器仍在运行（持有 service.lock）：请先停止它")

func (c *LegacyCompat) pairs() map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, t := range c.Targets {
		out[[2]string{t.Account, t.Plan}] = true
	}
	return out
}

func plainText(v string, limit int) bool {
	return v != "" && len(v) <= limit && !strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// checkLegacySnapshotSeed validates the opt-in section against the seed's own entries and the
// live state of the old directory. It returns nil, nil for an explicit opt-out. It never
// modifies anything and never puts an account into an error message.
func checkLegacySnapshotSeed(l *LegacySnapshotSeed, entries []TraceSeedEntry, now time.Time, heldDir string) (*LegacyCompat, error) {
	if !l.Enabled {
		if l.StateDir != "" || l.HistoryCount != 0 || len(l.Targets) != 0 {
			return nil, errors.New("legacy_snapshot.enabled=false 时不能携带其他字段")
		}
		return nil, nil
	}
	if !filepath.IsAbs(l.StateDir) || filepath.Clean(l.StateDir) != l.StateDir {
		return nil, errors.New("legacy_snapshot.state_dir 必须是规范的绝对路径")
	}
	if l.HistoryCount < 0 {
		return nil, errors.New("legacy_snapshot.history_count 无效")
	}
	if len(l.Targets) != len(entries) {
		return nil, errors.New("legacy_snapshot.targets 必须与种子条目一一对应")
	}
	byModel := map[string]TraceSeedEntry{}
	for _, e := range entries {
		byModel[e.Model] = e
	}
	compat := &LegacyCompat{StateDir: l.StateDir, HistoryBase: l.HistoryCount, Targets: map[string]LegacyCompatTarget{}}
	pairs := map[[2]string]string{}
	for _, t := range l.Targets {
		e, ok := byModel[t.Model]
		if !ok {
			return nil, fmt.Errorf("%s：legacy_snapshot 中的模型不在种子条目里", t.Model)
		}
		if _, dup := compat.Targets[t.Model]; dup {
			return nil, fmt.Errorf("%s：legacy_snapshot 中重复", t.Model)
		}
		if !plainText(t.Account, 256) || len(t.Plan) > 32 || strings.ContainsFunc(t.Plan, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("%s：账号或套餐无效", t.Model)
		}
		m := legacyModelRe.FindStringSubmatch(t.Model)
		sum := sha256.Sum256([]byte(t.Account))
		if m == nil || !strings.HasPrefix(hex.EncodeToString(sum[:]), m[1]) || e.Account != "sha256:"+m[1] {
			return nil, fmt.Errorf("%s：账号与模型 ID 中的哈希不对应", t.Model)
		}
		if e.Expect == "" || (e.Status == seedStatusMatch || e.Status == seedStatusMiss) && e.Prediction == "" {
			return nil, fmt.Errorf("%s：种子缺少期望候选或预测，无法生成快照", t.Model)
		}
		pair := [2]string{t.Account, t.Plan}
		if other, dup := pairs[pair]; dup {
			return nil, fmt.Errorf("%s 与 %s 映射到同一个旧账号", other, t.Model)
		}
		pairs[pair] = t.Model
		compat.Targets[t.Model] = LegacyCompatTarget{Account: t.Account, Plan: t.Plan, Expect: e.Expect}
	}
	if l.StateDir != heldDir { // this process' own writer already holds the old lock
		lock, err := lockLegacyCollector(l.StateDir)
		if err != nil {
			return nil, err
		}
		lock.Unlock() // only a probe: the writer takes the lock itself while it is active
	}
	if err := checkLegacySnapshotFile(l.StateDir, compat, now); err != nil {
		return nil, err
	}
	return compat, nil
}

// LegacySnapshotReport is the account-free summary shown next to a seed import.
func LegacySnapshotReport(seed TraceSeed) map[string]any {
	if seed.LegacySnapshot == nil {
		return nil
	}
	return map[string]any{"enabled": seed.LegacySnapshot.Enabled, "targets": len(seed.LegacySnapshot.Targets)}
}

// lockLegacyCollector takes the exclusive lock the old service.py holds for its whole life.
// Failure to get it means the collector is running (or the directory is not one).
func lockLegacyCollector(dir string) (*flock.Flock, error) {
	path := filepath.Join(dir, compatLockFile)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("旧采集器状态目录无效：找不到 service.lock")
	}
	l := flock.New(path)
	ok, err := l.TryLock()
	if err != nil {
		return nil, fmt.Errorf("无法检查旧采集器：%w", err)
	}
	if !ok {
		return nil, errLegacyRunning
	}
	return l, nil
}

type legacySnapshotFile struct {
	Version int `json:"version"`
	Records []struct {
		Account   string  `json:"account"`
		Plan      string  `json:"plan"`
		ExpiresAt float64 `json:"expiresAt"`
		Degraded  bool    `json:"degraded"`
	} `json:"records"`
}

// checkLegacySnapshotFile refuses to replace a snapshot it does not understand, or one with
// live records for accounts outside the mapping. An absent file is fine.
func checkLegacySnapshotFile(dir string, c *LegacyCompat, now time.Time) error {
	path := filepath.Join(dir, compatSnapshotFile)
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if d, e := os.Stat(dir); e != nil || !d.IsDir() {
			return errors.New("旧采集器状态目录不存在")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > compatMaxFile {
		return errors.New("现有 snapshot.json 不是普通文件或过大，拒绝覆盖")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cur legacySnapshotFile
	if json.Unmarshal(raw, &cur) != nil || cur.Version != compatSnapshotVersion {
		return errors.New("现有 snapshot.json 不是 version 1 格式，拒绝覆盖")
	}
	mapped, lost := c.pairs(), 0
	nowF := unixFloat(now)
	for _, r := range cur.Records {
		if (r.Degraded || r.ExpiresAt > nowF) && !mapped[[2]string{r.Account, r.Plan}] {
			lost++
		}
	}
	if lost > 0 {
		return fmt.Errorf("现有 snapshot.json 中有 %d 条有效记录属于未映射的账号，写入会丢失它们", lost)
	}
	return nil
}

func unixFloat(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

// compatRecord is one element of "records". The field set and order follow the old
// snapshot() exactly; nullable fields are pointers so they serialize as null.
type compatRecord struct {
	Provider          string   `json:"provider"`
	Source            string   `json:"source"`
	Account           string   `json:"account"`
	Plan              string   `json:"plan"`
	Model             string   `json:"model"`
	Mismatch          bool     `json:"mismatch"`
	At                float64  `json:"at"`
	ExpiresAt         float64  `json:"expiresAt"`
	LastAttemptFailed bool     `json:"lastAttemptFailed"`
	LastAttemptAt     float64  `json:"lastAttemptAt"`
	LastAttemptStatus *string  `json:"lastAttemptStatus"`
	Degraded          bool     `json:"degraded"`
	DegradedReason    *string  `json:"degradedReason"`
	DegradedAt        *float64 `json:"degradedAt"`
	RetryAfter        *float64 `json:"retryAfter"`
}

type compatSnapshot struct {
	Version      int            `json:"version"`
	GeneratedAt  float64        `json:"generatedAt"`
	Records      []compatRecord `json:"records"`
	HistoryCount int            `json:"historyCount"`
}

func ptr[T any](v T) *T { return &v }

// compatRecordFor derives the old record of one account from the model's run state, with
// the old snapshot() rules: the newest scored result stays for its own 6 hours; an
// unresolved 529 marker has no TTL (fixed finite expiresAt) and outlives it; the newest
// failed attempt after the scored result is the "last attempt". ok is false when neither a
// fresh result nor a marker exists, as the old collector then wrote no record.
func compatRecordFor(t LegacyCompatTarget, run *TraceRun, now time.Time) (compatRecord, bool) {
	if run == nil {
		return compatRecord{}, false
	}
	var fresh *TraceFingerprint
	if fp := run.Fingerprint; fp != nil && !fp.At.IsZero() && now.Sub(fp.At) <= traceStreakTTL {
		fresh = fp
	}
	marker := run.Streak529 > 0 && !run.At529.IsZero()
	if fresh == nil && !marker {
		return compatRecord{}, false
	}
	rec := compatRecord{Provider: compatProvider, Source: compatSource, Account: t.Account, Plan: t.Plan, Model: t.Expect}
	var at time.Time
	var status string
	if fresh != nil {
		at, status = fresh.At, "match"
		if fresh.Prediction != t.Expect {
			status = "mismatch"
		}
		rec.Mismatch = status == "mismatch"
	} else {
		at = run.At529 // 529 is not an analysis mismatch: Mismatch stays false
	}
	rec.At, rec.ExpiresAt = unixFloat(at), unixFloat(at.Add(traceStreakTTL))
	// The newest failed attempt that is newer than the scored result.
	var laterAt time.Time
	var laterStatus string
	switch run.Class {
	case class529:
		laterAt, laterStatus = run.ClassAt, "degraded"
		if laterAt.IsZero() {
			laterAt = run.At529
		}
	case classTransient, classUnknown:
		laterAt, laterStatus = run.ClassAt, "failed"
		if laterAt.IsZero() {
			laterAt = run.At
		}
	}
	if laterStatus != "" && (fresh == nil || laterAt.After(fresh.At)) {
		rec.LastAttemptAt, rec.LastAttemptStatus = unixFloat(laterAt), ptr(laterStatus)
		rec.LastAttemptFailed = true
	} else {
		rec.LastAttemptAt, rec.LastAttemptStatus = rec.At, ptr(status)
	}
	if marker {
		wait := trace529Waits[min(run.Streak529, len(trace529Waits))-1]
		rec.Degraded, rec.DegradedReason = true, ptr("http_529")
		rec.DegradedAt, rec.RetryAfter = ptr(unixFloat(run.At529)), ptr(unixFloat(run.At529.Add(wait)))
		rec.ExpiresAt = compatDegradedExpiresAt
	}
	return rec, true
}

// buildCompatSnapshot assembles the whole file content for the mapped models.
func buildCompatSnapshot(c *LegacyCompat, runs map[string]*TraceRun, now time.Time) compatSnapshot {
	out := compatSnapshot{Version: compatSnapshotVersion, GeneratedAt: unixFloat(now), Records: []compatRecord{}, HistoryCount: c.HistoryBase + c.HistoryAdded}
	models := make([]string, 0, len(c.Targets))
	for m := range c.Targets {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		if rec, ok := compatRecordFor(c.Targets[m], runs[m], now); ok {
			out.Records = append(out.Records, rec)
		}
	}
	// Newest attempt first, as the old rows were grouped.
	sort.SliceStable(out.Records, func(i, j int) bool { return out.Records[i].LastAttemptAt > out.Records[j].LastAttemptAt })
	return out
}

var statusUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.:/-]`)

func statusSafe(v string, limit int) string {
	r := []rune(statusUnsafe.ReplaceAllString(v, "?"))
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}

func statusWhen(t time.Time) string { return t.In(legacyJST).Format("01-02 15:04") + " JST" }

func statusSpan(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.Itoa(int(d/time.Hour)) + " 小时"
	}
	return strconv.Itoa(int(math.Round(float64(d)/float64(time.Minute)))) + " 分钟"
}

// statusMask is the old _mask(): two characters of the local part, the domain, and four hex
// characters of the account hash. The full account never appears in status.md.
func statusMask(account, plan string) string {
	local, domain, hasAt := strings.Cut(account, "@")
	lr := []rune(local)
	if len(lr) > 2 {
		lr = lr[:2]
	}
	name := statusSafe(string(lr), 48) + "***"
	if hasAt {
		name += "@" + statusSafe(domain, 32)
	}
	sum := sha256.Sum256([]byte(account))
	p := statusSafe(plan, 16)
	if p == "" {
		p = "-"
	}
	return fmt.Sprintf("%s #%s / %s", name, hex.EncodeToString(sum[:])[:4], p)
}

// buildCompatStatus renders status.md in the old table, from the new schedule's own state.
func buildCompatStatus(c *LegacyCompat, runs map[string]*TraceRun, now time.Time) string {
	type row struct{ key, text string }
	var rows []row
	for model, t := range c.Targets {
		run := runs[model]
		if run == nil {
			run = &TraceRun{}
		}
		latest := "无尝试"
		var newest time.Time
		why := ""
		status := ""
		if fp := run.Fingerprint; fp != nil {
			newest, status, why = fp.At, "match", fp.Prediction
			if fp.Prediction != t.Expect {
				status = "mismatch"
			}
		}
		switch run.Class {
		case class529, classTransient, classUnknown:
			at := run.ClassAt
			if at.IsZero() {
				at = run.At
			}
			if at.After(newest) {
				newest = at
				switch run.Class {
				case class529:
					status, why = "degraded", legacyDegradedReason
				default:
					status, why = "failed", run.Class
				}
			}
		}
		if status != "" {
			latest = statusSafe(status, 16) + " · " + statusWhen(newest)
			if why != "" {
				latest += " · " + statusSafe(why, 40)
			}
		}
		marker := run.Streak529 > 0 && !run.At529.IsZero()
		if marker {
			latest = "被降智 · " + statusWhen(run.At529) + " · 最新 " + latest
		}
		streak := strconv.Itoa(traceStreakAt(run, now))
		interval, next := traceCadence(traceStreakAt(run, now)), "可测"
		switch {
		case run.Paused:
			rows = append(rows, row{t.Account + "\x00" + t.Plan, fmt.Sprintf("| %s | %s | %s | 暂停 | 暂停（未决失败，不自动继续） |", statusMask(t.Account, t.Plan), latest, streak)})
			continue
		case marker:
			interval = trace529Waits[min(run.Streak529, len(trace529Waits))-1]
		case run.Class == classTransient && run.Transient > 0:
			interval = traceTransientWaits[min(run.Transient, len(traceTransientWaits))-1]
		}
		if due := traceDueAt(run, now); !due.IsZero() {
			next = statusWhen(due)
			if !due.After(now) {
				next = "可测（仍需满足额度门禁）"
			}
		}
		rows = append(rows, row{t.Account + "\x00" + t.Plan, fmt.Sprintf("| %s | %s | %s | %s | %s |", statusMask(t.Account, t.Plan), latest, streak, statusSpan(interval), next)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	lines := []string{"| 账号 | 最新结果 | 连续一致 | 当前间隔 | 下次可测 |", "| --- | --- | --- | --- | --- |"}
	for _, r := range rows {
		lines = append(lines, r.text)
	}
	return strings.Join(lines, "\n") + "\n"
}

// compatRuntime is the writer's process-local state. mu serializes whole refreshes so two
// triggers can never write the old directory concurrently.
type compatRuntime struct {
	mu        sync.Mutex
	wake      chan struct{}
	lock      *flock.Flock
	lockDir   string // old state directory the lock belongs to
	lastBody  []byte // content of the last write without generatedAt
	lastWrite time.Time
	status    LegacyCompatState
}

// LegacyCompatState is the account-free view for the console.
type LegacyCompatState struct {
	Configured bool      `json:"configured"`
	Active     bool      `json:"active"`
	Reason     string    `json:"reason,omitempty"` // why the writer is idle or blocked
	Targets    int       `json:"targets"`
	LastWrite  time.Time `json:"last_write,omitzero"`
	Writes     int       `json:"writes"`
	Error      string    `json:"error,omitempty"`
}

func (s *Service) LegacyCompatStatus() LegacyCompatState {
	s.compat.mu.Lock()
	defer s.compat.mu.Unlock()
	st := s.compat.status
	s.mu.Lock()
	st.Configured = s.state.Compat != nil
	s.mu.Unlock()
	return st
}

func (s *Service) wakeCompat() {
	select {
	case s.compat.wake <- struct{}{}:
	default:
	}
}

func (s *Service) releaseLegacyLock() {
	s.compat.mu.Lock()
	defer s.compat.mu.Unlock()
	s.releaseLegacyLockLocked()
}

func (s *Service) releaseLegacyLockLocked() {
	if s.compat.lock != nil {
		s.compat.lock.Unlock()
		s.compat.lock, s.compat.lockDir = nil, ""
	}
}

// heldLegacyDir is the old state directory whose lock this process holds, if any.
func (s *Service) heldLegacyDir() string {
	s.compat.mu.Lock()
	defer s.compat.mu.Unlock()
	return s.compat.lockDir
}

func (s *Service) startLegacyCompat() {
	s.compat.wake = make(chan struct{}, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			var timer *time.Timer
			var fire <-chan time.Time
			if wait := s.refreshLegacyCompat(s.traceNow()); wait > 0 {
				timer = time.NewTimer(wait)
				fire = timer.C
			}
			select {
			case <-s.ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-s.compat.wake:
			case <-fire:
			}
			if timer != nil {
				timer.Stop()
			}
		}
	}()
}

// refreshLegacyCompat rewrites the old snapshot (and status.md) if, and only if, the opt-in
// gates hold. It returns how long to wait before looking again; 0 means idle until a wake.
func (s *Service) refreshLegacyCompat(now time.Time) time.Duration {
	rt := &s.compat
	rt.mu.Lock()
	defer rt.mu.Unlock()

	s.mu.Lock()
	var c *LegacyCompat
	runs := map[string]*TraceRun{}
	reason := ""
	if sc := s.state.Compat; sc != nil {
		cc := *sc
		cc.Targets = make(map[string]LegacyCompatTarget, len(sc.Targets))
		for m, t := range sc.Targets {
			cc.Targets[m] = t
			if r := s.state.TraceSchedule.Runs[m]; r != nil {
				rc := *r
				if r.Fingerprint != nil {
					fp := *r.Fingerprint
					rc.Fingerprint = &fp
				}
				runs[m] = &rc
			}
		}
		c = &cc
		reason = s.compatGateLocked(&cc)
	}
	s.mu.Unlock()

	rt.status.Targets = 0
	if c == nil {
		s.releaseLegacyLockLocked()
		rt.status = LegacyCompatState{Reason: "未启用：种子导入时没有 legacy_snapshot 选项", Writes: rt.status.Writes, LastWrite: rt.status.LastWrite}
		return 0
	}
	rt.status.Targets = len(c.Targets)
	block := func(why string, retry time.Duration) time.Duration {
		rt.status.Active, rt.status.Reason = false, why
		return retry
	}
	if reason != "" {
		s.releaseLegacyLockLocked() // schedule off: the old collector may be started again
		rt.lastBody = nil
		return block(reason, 0)
	}
	if rt.lock == nil {
		l, err := lockLegacyCollector(c.StateDir)
		if err != nil {
			return block(err.Error(), compatTick)
		}
		rt.lock, rt.lockDir = l, c.StateDir
	}
	if err := checkLegacySnapshotFile(c.StateDir, c, now); err != nil {
		return block(err.Error(), compatTick)
	}
	snap := buildCompatSnapshot(c, runs, now)
	snap.GeneratedAt = 0
	body, err := json.Marshal(snap)
	if err != nil {
		return block(err.Error(), compatTick)
	}
	if bytes.Equal(body, rt.lastBody) && now.Sub(rt.lastWrite) < compatHeartbeat {
		rt.status.Active, rt.status.Reason, rt.status.Error = true, "", ""
		return compatTick
	}
	snap.GeneratedAt = unixFloat(now)
	out, _ := json.Marshal(snap)
	if err = AtomicWrite(filepath.Join(c.StateDir, compatSnapshotFile), out); err != nil {
		rt.status.Error = "写入 snapshot.json 失败：" + err.Error()
		return block(rt.status.Error, compatTick)
	}
	rt.lastBody, rt.lastWrite = body, now
	rt.status.Active, rt.status.Reason, rt.status.Error = true, "", ""
	rt.status.LastWrite, rt.status.Writes = now, rt.status.Writes+1
	if err = AtomicWrite(filepath.Join(c.StateDir, compatStatusFile), []byte(buildCompatStatus(c, runs, now))); err != nil {
		rt.status.Error = "写入 status.md 失败：" + err.Error()
	}
	return compatTick
}

// compatGateLocked returns why the writer must stay idle ("" means it may write).
func (s *Service) compatGateLocked(c *LegacyCompat) string {
	sch := &s.state.TraceSchedule
	if !sch.Enabled {
		return "定时检测未启用"
	}
	for model, t := range c.Targets {
		plan, scheduled := sch.Models[model]
		switch {
		case !scheduled:
			return "模型未加入定时检测：" + model
		case s.state.Targets[model] == nil || !s.state.Targets[model].Enabled:
			return "监测目标未启用：" + model
		case fingerprintModelFor(model, plan.Expect) != t.Expect:
			return "定时检测的期望候选与种子不一致：" + model
		}
	}
	return ""
}
