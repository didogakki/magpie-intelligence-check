package check

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reader for the state of the old magpie-modeltrace/service.py collector. It never
// writes there: the SQLite file is copied to a private temporary directory and the copy
// is opened. History rows and the run logs they point to are the source of truth; the
// optional status.md is only compared against the result as a display check.
const (
	legacyDegradedReason = "inference:unknown:http_529"
	legacyMaxLog         = 16 << 20
)

// A model ID that stands for one old account: <provider>-<first 8 hex of sha256(account)>/...
// The plugin sees only model IDs, so this hash prefix is the whole account mapping.
var legacyModelRe = regexp.MustCompile(`^[^/]*-([0-9a-f]{8})/`)

type legacyRow struct {
	at                     float64
	status, prediction, id string
	path, detail           string
}

func unixTime(f float64) time.Time {
	return time.Unix(0, int64(f*1e9)).UTC()
}

// ExportLegacySeed builds a seed for each model from the old collector's state directory.
// It returns display-check warnings next to the seed; any error means nothing usable.
func ExportLegacySeed(stateDir string, models []string, statusMD string, now time.Time) (TraceSeed, []string, error) {
	return exportLegacySeed(stateDir, models, statusMD, now, false)
}

// ExportLegacySeedWithSnapshot is ExportLegacySeed plus the explicit opt-in section for the
// compatibility snapshot writer. The result holds the old account identifiers: write it to a
// private file (0600) and never print it.
func ExportLegacySeedWithSnapshot(stateDir string, models []string, statusMD string, now time.Time) (TraceSeed, []string, error) {
	return exportLegacySeed(stateDir, models, statusMD, now, true)
}

func exportLegacySeed(stateDir string, models []string, statusMD string, now time.Time, snapshot bool) (TraceSeed, []string, error) {
	if snapshot && (!filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir) {
		return TraceSeed{}, nil, errors.New("兼容快照需要规范的绝对旧状态目录")
	}
	db, cleanup, err := openLegacyCopy(stateDir)
	if err != nil {
		return TraceSeed{}, nil, err
	}
	defer cleanup()
	seed := TraceSeed{Version: traceSeedVersion, Source: "magpie-modeltrace/service.py", ExportedAt: now.UTC()}
	var warnings []string
	if snapshot {
		seed.LegacySnapshot = &LegacySnapshotSeed{Enabled: true, StateDir: stateDir}
		if err = db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&seed.LegacySnapshot.HistoryCount); err != nil {
			return TraceSeed{}, nil, err
		}
	}
	for _, model := range models {
		entry, target, w, err := legacyEntry(db, model, now)
		if err != nil {
			return TraceSeed{}, nil, err
		}
		if snapshot {
			seed.LegacySnapshot.Targets = append(seed.LegacySnapshot.Targets, target)
		}
		warnings = append(warnings, w...)
		if statusMD != "" {
			warnings = append(warnings, checkLegacyStatus(statusMD, entry, now)...)
		}
		seed.Entries = append(seed.Entries, entry)
	}
	return seed, warnings, nil
}

func openLegacyCopy(stateDir string) (*sql.DB, func(), error) {
	dir, err := os.MkdirTemp("", "legacy-trace-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(filepath.Join(stateDir, "history.sqlite"+suffix))
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if err = os.WriteFile(filepath.Join(dir, "history.sqlite"+suffix), raw, 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.sqlite"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	return db, func() { db.Close(); cleanup() }, nil
}

func legacyEntry(db *sql.DB, model string, now time.Time) (TraceSeedEntry, LegacySnapshotTarget, []string, error) {
	var none LegacySnapshotTarget
	m := legacyModelRe.FindStringSubmatch(model)
	if m == nil {
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：模型 ID 不含账号哈希（需形如 provider-xxxxxxxx/...）", model)
	}
	hash8 := m[1]
	expect := fingerprintModelFor(model, "")
	if expect == "" {
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：指纹库中没有对应候选", model)
	}
	accounts, err := legacyAccounts(db, hash8)
	if err != nil {
		return TraceSeedEntry{}, none, nil, err
	}
	if len(accounts) != 1 {
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：旧历史中匹配哈希 %s 的账号有 %d 个，必须恰好 1 个", model, hash8, len(accounts))
	}
	account, plan := accounts[0][0], accounts[0][1]
	target := LegacySnapshotTarget{Model: model, Account: account, Plan: plan}
	rows, err := legacyRows(db, account, plan)
	if err != nil || len(rows) == 0 {
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：旧历史没有记录 %v", model, err)
	}
	entry := TraceSeedEntry{Model: model, Account: "sha256:" + hash8, Expect: expect}
	var warnings []string
	newest := rows[0]
	entry.ResultAt = unixTime(newest.at)
	switch {
	case newest.status == "match":
		entry.Status, entry.Prediction = seedStatusMatch, newest.prediction
		for _, r := range rows {
			if r.status != "match" {
				break
			}
			if r.prediction != expect {
				return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：历史 match 的预测 %q 不是 %s", model, r.prediction, expect)
			}
			w, err := verifyLegacyLog(r, expect)
			if err != nil {
				return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：%w", model, err)
			}
			warnings = append(warnings, w...)
			entry.Streak++
		}
		entry.LastMatchAt = entry.ResultAt
	case newest.status == "mismatch":
		entry.Status, entry.Prediction = seedStatusMiss, newest.prediction
	case legacyDegraded(newest):
		entry.Status = seedStatus529
		for _, r := range rows {
			if !legacyDegraded(r) || entry.Streak529 >= len(trace529Waits) {
				break
			}
			entry.Streak529++
		}
		entry.At529 = entry.ResultAt
	default:
		entry.Status = seedStatusOther
	}
	if entry.Status == seedStatus529 || entry.Status == seedStatusOther {
		for _, r := range rows { // newest scored result, which stays visible for its own TTL
			if r.status == "match" || r.status == "mismatch" {
				entry.ScoredAt, entry.ScoredPrediction = unixTime(r.at), r.prediction
				break
			}
		}
		if entry.ScoredPrediction == "" {
			entry.ScoredAt = time.Time{}
		}
	}
	attempt, pending, found, err := legacyAttempt(db, account, plan)
	if err != nil {
		return TraceSeedEntry{}, none, nil, err
	}
	switch {
	case pending:
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：旧采集器还有未决的尝试，请等它结束后再导出", model)
	case found:
		entry.AttemptAt = unixTime(attempt)
	default:
		entry.AttemptAt = entry.ResultAt
		warnings = append(warnings, model+"：旧历史没有 attempts 记录，以最近结果时间作为起点")
	}
	if entry.AttemptAt.After(now.Add(time.Minute)) || entry.ResultAt.After(now.Add(time.Minute)) {
		return TraceSeedEntry{}, none, nil, fmt.Errorf("%s：旧历史时间在未来", model)
	}
	return entry, target, warnings, nil
}

func legacyAccounts(db *sql.DB, hash8 string) ([][2]string, error) {
	rs, err := db.Query(`SELECT DISTINCT account, plan FROM history WHERE source='252'`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out [][2]string
	for rs.Next() {
		var account, plan string
		if err = rs.Scan(&account, &plan); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(account))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), hash8) {
			out = append(out, [2]string{account, plan})
		}
	}
	return out, rs.Err()
}

func legacyRows(db *sql.DB, account, plan string) ([]legacyRow, error) {
	rs, err := db.Query(`SELECT at, status, COALESCE(prediction,''), id, COALESCE(path,''), COALESCE(detail,'') FROM history WHERE account=? AND plan=? AND source='252' ORDER BY at DESC`, account, plan)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []legacyRow
	for rs.Next() {
		var r legacyRow
		if err = rs.Scan(&r.at, &r.status, &r.prediction, &r.id, &r.path, &r.detail); err != nil {
			return nil, err
		}
		if r.status != "quota" { // quota rows never interrupt a streak
			out = append(out, r)
		}
	}
	return out, rs.Err()
}

// legacyDegraded mirrors the old collector's degraded(): the row detail alone is never
// trusted. The run log must close with a summary that says unknown / http_529 / status 529
// and its final terminal record must be the inference one with the same 529. Anything
// else, including an unreadable log, is not degraded and stays an unconfirmed outcome.
func legacyDegraded(r legacyRow) bool {
	if r.status != "failed" {
		return false
	}
	var d struct {
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal([]byte(r.detail), &d) != nil || d.StopReason != legacyDegradedReason {
		return false
	}
	raw, ok := readLegacyLog(r.path)
	if !ok {
		return false
	}
	is529 := func(v json.RawMessage) bool {
		var o struct {
			HTTPStatus json.Number `json:"http_status"`
		}
		return json.Unmarshal(v, &o) == nil && o.HTTPStatus.String() == "529"
	}
	var summary struct {
		Status     string          `json:"status"`
		StopReason string          `json:"stop_reason"`
		StopDetail json.RawMessage `json:"stop_detail"`
	}
	closed := false
	var last struct {
		RequestKind string          `json:"request_kind"`
		Outcome     string          `json:"outcome"`
		Reason      string          `json:"reason"`
		Detail      json.RawMessage `json:"detail"`
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var ev struct {
			Type        string          `json:"type"`
			RequestKind string          `json:"request_kind"`
			Outcome     string          `json:"outcome"`
			Reason      string          `json:"reason"`
			Detail      json.RawMessage `json:"detail"`
			Summary     json.RawMessage `json:"summary"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "run_end":
			closed = json.Unmarshal(ev.Summary, &summary) == nil
		case "attempt_terminal":
			last.RequestKind, last.Outcome, last.Reason, last.Detail = ev.RequestKind, ev.Outcome, ev.Reason, ev.Detail
		}
	}
	return closed && summary.Status == "unknown" && summary.StopReason == legacyDegradedReason && is529(summary.StopDetail) &&
		last.RequestKind == "inference" && last.Outcome == "unknown" && last.Reason == "http_529" && is529(last.Detail)
}

// readLegacyLog reads the single run log a history row points to (a file, or a run
// directory holding exactly one run-*.jsonl).
func readLegacyLog(path string) ([]byte, bool) {
	if st, err := os.Lstat(path); err == nil && st.IsDir() {
		logs, _ := filepath.Glob(filepath.Join(path, "run-*.jsonl"))
		if len(logs) != 1 {
			return nil, false
		}
		path = logs[0]
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > legacyMaxLog {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	return raw, err == nil
}

func legacyAttempt(db *sql.DB, account, plan string) (at float64, pending, found bool, err error) {
	rs, err := db.Query(`SELECT identity, at, pending FROM attempts`)
	if err != nil {
		return 0, false, false, err
	}
	defer rs.Close()
	for rs.Next() {
		var ident string
		var p int
		var a float64
		if err = rs.Scan(&ident, &a, &p); err != nil {
			return 0, false, false, err
		}
		var pair []string
		if json.Unmarshal([]byte(ident), &pair) == nil && len(pair) == 2 && pair[0] == account && pair[1] == plan {
			return a, p != 0, true, nil
		}
	}
	return 0, false, false, rs.Err()
}

// verifyLegacyLog checks one streak row against the run log it came from. A missing log
// only warns (the history row was written from it); a log that contradicts the row is an
// error. The answers in the log are also re-scored with this plugin's scorer.
func verifyLegacyLog(r legacyRow, expect string) ([]string, error) {
	path := r.path
	if st, err := os.Lstat(path); err == nil && st.IsDir() {
		logs, _ := filepath.Glob(filepath.Join(path, "run-*.jsonl"))
		if len(logs) != 1 {
			return []string{r.id + "：旧运行目录中的日志不唯一，已跳过核对"}, nil
		}
		path = logs[0]
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > legacyMaxLog {
		return []string{r.id + "：旧运行日志不可读，已跳过核对"}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{r.id + "：旧运行日志不可读，已跳过核对"}, nil
	}
	var summary struct {
		Status string `json:"status"`
		Counts struct {
			Valid int `json:"transport_valid_samples"`
		} `json:"counts"`
		Analysis struct {
			Prediction string `json:"prediction"`
		} `json:"mt_analysis_raw"`
		Target struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"target"`
	}
	closed := false
	var outputs []FingerprintOutput
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Type        string          `json:"type"`
			RequestKind string          `json:"request_kind"`
			Outcome     string          `json:"outcome"`
			Summary     json.RawMessage `json:"summary"`
			Detail      struct {
				Text  string `json:"text"`
				Count int    `json:"expected_count"`
			} `json:"detail"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "run_end" && json.Unmarshal(ev.Summary, &summary) == nil:
			closed = true
		case ev.Type == "attempt_terminal" && ev.RequestKind == "inference" && ev.Outcome == "sample_transport_valid":
			outputs = append(outputs, FingerprintOutput{Text: ev.Detail.Text, ExpectedCount: ev.Detail.Count})
		}
	}
	if !closed || summary.Status != "analysis_received_unverified" || summary.Counts.Valid != 3 ||
		summary.Analysis.Prediction != expect || summary.Target.Provider != "codex" || summary.Target.Model != "codex/"+expect {
		return nil, fmt.Errorf("旧运行日志与历史 match 记录矛盾：%s", r.id)
	}
	var warnings []string
	if report, err := AnalyzeFingerprint(outputs); err != nil || len(outputs) != 3 || report.UsedOutputs != 3 || report.Prediction != expect {
		warnings = append(warnings, r.id+"：用本插件评分重新计算与旧结果不一致，仍按旧日志结论计入")
	}
	return warnings, nil
}

var legacyJST = time.FixedZone("JST", 9*3600)

// checkLegacyStatus compares the derived values with the old status.md row of the same
// account (matched by the 4 hex characters it prints). Differences are warnings: the
// file is a display and may simply be older than the database.
func checkLegacyStatus(path string, e TraceSeedEntry, now time.Time) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"status.md 不可读，已跳过显示核对"}
	}
	tag := "#" + strings.TrimPrefix(e.Account, "sha256:")[:4]
	for _, line := range strings.Split(string(raw), "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 5 || !strings.Contains(cells[0], tag) {
			continue
		}
		run := seedRun(e, "", nil)
		cadence := traceCadence(traceStreakAt(run, now))
		span := strconv.Itoa(int(cadence/time.Minute)) + " 分钟"
		if cadence%time.Hour == 0 {
			span = strconv.Itoa(int(cadence/time.Hour)) + " 小时"
		}
		var out []string
		if got := strings.TrimSpace(cells[2]); got != strconv.Itoa(e.Streak) {
			out = append(out, fmt.Sprintf("%s：status.md 连续一致 %s，数据库 %d", e.Model, got, e.Streak))
		}
		if got := strings.TrimSpace(cells[3]); got != span {
			out = append(out, fmt.Sprintf("%s：status.md 间隔 %s，推导 %s", e.Model, got, span))
		}
		if want := e.AttemptAt.Add(cadence).In(legacyJST).Format("01-02 15:04") + " JST"; strings.TrimSpace(cells[4]) != want && !strings.HasPrefix(strings.TrimSpace(cells[4]), "可测") {
			out = append(out, fmt.Sprintf("%s：status.md 下次可测 %s，推导 %s", e.Model, strings.TrimSpace(cells[4]), want))
		}
		return out
	}
	return []string{e.Model + "：status.md 中没有 " + tag + " 行"}
}
