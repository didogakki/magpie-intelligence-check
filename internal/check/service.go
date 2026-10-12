package check

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	GatewayURL string `json:"gateway_url"`
	GatewayKey string `json:"gateway_key"`
	LogPath    string `json:"log_path"`
	DailyLimit int    `json:"daily_limit"`
}
type Target struct {
	Model      string    `json:"model"`
	Enabled    bool      `json:"enabled"`
	State      string    `json:"state"`
	CheckedAt  time.Time `json:"checked_at"`
	AllowUntil time.Time `json:"allow_until"`
	RetryAfter time.Time `json:"retry_after"`
	Reason     string    `json:"reason"`
	Event      string    `json:"event"`
}
type Result struct {
	ID            string    `json:"id"`
	Model         string    `json:"model"`
	Prompt        string    `json:"prompt"`
	Answer        string    `json:"answer"`
	Verdict       string    `json:"verdict"`
	Error         string    `json:"error,omitempty"`
	HTTPStatus    int       `json:"http_status,omitempty"` // real upstream status of a failed trace probe
	ErrorKind     string    `json:"error_kind,omitempty"`  // "timeout" or "network": confirmed transport failure
	Time          time.Time `json:"time"`
	Tokens        int       `json:"tokens"`
	Batch         string    `json:"batch,omitempty"`
	ExpectedCount int       `json:"expected_count,omitempty"`
	PromptText    string    `json:"prompt_text,omitempty"`
	Review        string    `json:"review,omitempty"`
	ReviewNote    string    `json:"review_note,omitempty"`
	ReviewedAt    time.Time `json:"reviewed_at,omitempty"`
}

func (s *Service) ReviewResult(id, decision, note string) error {
	if decision != "accepted" && decision != "rejected" && decision != "pending" {
		return errors.New("无效审核结论")
	}
	if len(note) > 4000 {
		return errors.New("审核备注不能超过 4000 字节")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Results {
		r := &s.state.Results[i]
		if r.ID != id {
			continue
		}
		if r.Verdict == "running" || r.Answer == "" {
			return errors.New("尚无可审核的回答")
		}
		previous := *r
		r.Review, r.ReviewNote, r.ReviewedAt = decision, note, time.Now()
		if err := s.store.Save(s.state); err != nil {
			*r = previous
			return err
		}
		return nil
	}
	return errors.New("检测记录不存在")
}

func (s *Service) DeleteTarget(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[model] {
		return errors.New("模型正在检测，请完成后再删除")
	}
	t, ok := s.state.Targets[model]
	if !ok {
		return errors.New("模型未配置")
	}
	delete(s.state.Targets, model)
	if err := s.saveLocked(); err != nil {
		s.state.Targets[model] = t
		return err
	}
	return nil
}

type State struct {
	Config        Config                    `json:"config"`
	Targets       map[string]*Target        `json:"targets"`
	Results       []Result                  `json:"results"`
	Alias         string                    `json:"alias"`
	ClearedUsage  map[string]map[string]int `json:"cleared_usage,omitempty"`
	TraceSchedule TraceSchedule             `json:"trace_schedule"`
	Compat        *LegacyCompat             `json:"legacy_compat,omitempty"` // private: holds old account identifiers
}

func (s *Service) ClearResults(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) > 0 {
		return errors.New("检测进行中，请完成或取消后再清理记录")
	}
	selected := make(map[string]bool)
	for _, id := range ids {
		selected[id] = true
	}
	previous, _ := json.Marshal(s.state)
	if s.state.ClearedUsage == nil {
		s.state.ClearedUsage = make(map[string]map[string]int)
	}
	kept := make([]Result, 0, len(s.state.Results))
	for _, result := range s.state.Results {
		if !selected[result.ID] {
			kept = append(kept, result)
			continue
		}
		day := result.Time.UTC().Format("2006-01-02")
		if s.state.ClearedUsage[day] == nil {
			s.state.ClearedUsage[day] = make(map[string]int)
		}
		s.state.ClearedUsage[day][result.Model]++
	}
	s.state.Results = kept
	if err := s.store.Save(s.state); err != nil {
		json.Unmarshal(previous, &s.state)
		return err
	}
	return nil
}

type Service struct {
	mu           sync.Mutex
	state        State
	dir, gate    string
	running      map[string]bool
	testCancels  map[string]context.CancelFunc
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	store        *Store
	health       string
	lastActivity time.Time
	schedWake    chan struct{}
	schedOnce    sync.Once
	compat       compatRuntime
	clock        func() time.Time // nil means time.Now; tests inject a fixed clock
}

func Secret() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func New(dir, gate string) (*Service, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(dir, "check.db"))
	if err != nil {
		return nil, err
	}
	s := &Service{dir: dir, gate: gate, store: store, running: map[string]bool{}, schedWake: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.state = State{Config: Config{DailyLimit: 120}, Targets: map[string]*Target{}, Results: []Result{}, Alias: Secret()}
	if err := store.Load(&s.state); err != nil {
		store.Close()
		return nil, err
	}
	for _, t := range s.state.Targets {
		if t.State == "checking" {
			t.State = "unavailable"
			t.Reason = "检测服务重启，请重试"
		}
	}
	for i := range s.state.Results {
		if s.state.Results[i].Verdict == "running" {
			s.state.Results[i].Verdict = "unavailable"
			s.state.Results[i].Error = "检测服务重启，请重试"
		}
	}
	s.normalizeScheduleLocked()
	if err := s.saveLocked(); err != nil {
		store.Close()
		return nil, err
	}
	return s, nil
}
func (s *Service) Close() { s.cancel(); s.wg.Wait(); s.releaseLegacyLock(); s.store.Close() }
func AtomicWrite(name string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".quality-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
func (s *Service) saveLocked() error {
	if err := s.store.Save(s.state); err != nil {
		s.health = err.Error()
		return err
	}
	b, _ := json.Marshal(s.state.Targets)
	a, _ := json.Marshal(s.state.Alias)
	// Rewrite the entrypoint: Magpie does not watch imported modules.
	code := fmt.Sprintf(`const targets=%s;
const alias=%s;
export function onRequest(body,ctx){
 if(typeof body.model!=="string")return body;
 const prefix="quality-probe/"+alias+"/";
 if(body.model.startsWith(prefix)){body.model=body.model.slice(prefix.length);return body;}
 const t=targets[body.model];if(!t||!t.enabled)return body;
 const now=Date.now();
 if(Date.parse(t.allow_until)>now)return body;
 if(t.state==="passed"&&now-Date.parse(t.checked_at)<900000)return body;
 if(t.state!=="review"&&t.state!=="checking"&&!(Date.parse(t.retry_after)>now))console.log("MAGPIE_QUALITY_TRIGGER "+JSON.stringify({model:body.model}));
 const status=t.state==="review"?"需要人工确认":t.state==="checking"?"检测进行中，请稍后重试":"需要检测；自动触发依赖可用日志源，请查看检测控制台";
 return ctx.reject(403,"[MAGPIE_QUALITY_REVIEW_REQUIRED] "+status+"。本次请求未发往上游。目标："+body.model+"；原因："+(t.reason||"首次使用或检测已过期")+"；事件："+(t.event||"无")+"。请重新检测、限时放行或切换模型。");
}
`, b, a)
	if err := AtomicWrite(s.gate, []byte(code)); err != nil {
		s.health = "门禁发布失败：" + err.Error()
		return err
	}
	s.health = ""
	return nil
}
func (s *Service) Snapshot() (State, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var out State
	json.Unmarshal(b, &out)
	out.Config.GatewayKey = ""
	out.Alias = ""
	out.Compat = nil // the console never sees the old account identifiers; see LegacyCompatStatus
	return out, s.health
}
func (s *Service) Configure(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configureLocked(c)
}
func (s *Service) ConfigureInitial(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Config.GatewayURL != "" {
		return nil
	}
	return s.configureLocked(c)
}
func (s *Service) configureLocked(c Config) error {
	if c.GatewayKey == "" {
		c.GatewayKey = s.state.Config.GatewayKey
	}
	if c.DailyLimit < 1 || c.DailyLimit > 10000 {
		return errors.New("每日预算须为 1–10000 次")
	}
	if err := ValidateGateway(c.GatewayURL); err != nil {
		return err
	}
	if c.GatewayKey == "" {
		return errors.New("请输入 Magpie 网关 Key；本机默认 Key 为 magpie")
	}
	s.state.Config = c
	return s.saveLocked()
}
func (s *Service) SetTarget(model string, enabled bool) error {
	if model == "" || len(model) > 512 || strings.ContainsAny(model, "\r\n") || strings.HasPrefix(model, "quality-probe/") {
		return errors.New("无效模型 ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.state.Targets[model]
	if t == nil {
		t = &Target{Model: model, State: "unverified"}
		s.state.Targets[model] = t
	}
	t.Enabled = enabled
	return s.saveLocked()
}
func (s *Service) Action(model, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.state.Targets[model]
	if t == nil {
		return errors.New("模型未配置")
	}
	switch action {
	case "allow":
		t.AllowUntil = time.Now().Add(15 * time.Minute)
	case "hold":
		t.AllowUntil = time.Time{}
		t.State = "review"
		t.Reason = "人工暂停"
	case "disable":
		t.Enabled = false
	default:
		return errors.New("未知操作")
	}
	return s.saveLocked()
}
func (s *Service) Trigger(model, prompt string, manual bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.state.Targets[model]
	if t == nil {
		return errors.New("模型未配置")
	}
	if !t.Enabled && !manual {
		return nil
	}
	if s.running[model] {
		if manual {
			return errors.New("该模型已有检测进行中，请完成后重试")
		}
		return nil
	}
	if !manual && (t.State == "review" || t.AllowUntil.After(time.Now()) || t.State == "passed" && time.Since(t.CheckedAt) < 15*time.Minute) {
		return nil
	}
	if !manual && t.RetryAfter.After(time.Now()) {
		return nil
	}
	p, ok := FindPrompt(prompt)
	if !ok {
		return errors.New("未知题目")
	}
	if s.state.Config.GatewayURL == "" || s.state.Config.GatewayKey == "" {
		return errors.New("请配置 Magpie 网关 URL 和 Key")
	}
	count := 0
	day := time.Now().UTC().Format("2006-01-02")
	count += s.state.ClearedUsage[day][model]
	for _, r := range s.state.Results {
		if r.Model == model && r.Time.UTC().Format("2006-01-02") == day {
			count++
		}
	}
	if count >= s.state.Config.DailyLimit {
		return errors.New("每日请求预算已耗尽")
	}
	previous := t.State
	if p.Automatic {
		t.State = "checking"
		t.Reason = ""
		t.Event = Secret()[:12]
	}
	if err := s.saveLocked(); err != nil {
		t.State = previous
		return err
	}
	s.running[model] = true
	if s.testCancels == nil {
		s.testCancels = make(map[string]context.CancelFunc)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 180*time.Second)
	s.testCancels[model] = cancel
	s.wg.Add(1)
	go s.run(ctx, model, p)
	return nil
}
func (s *Service) CancelTest(model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cancel := s.testCancels[model]
	if cancel == nil {
		return errors.New("该模型没有可取消的质量检测")
	}
	cancel()
	return nil
}
func (s *Service) run(ctx context.Context, model string, p Prompt) {
	defer s.wg.Done()
	verdict, reason := "unavailable", ""
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.Lock()
		cfg := s.state.Config
		alias := s.state.Alias
		count := 0
		day := time.Now().UTC().Format("2006-01-02")
		count += s.state.ClearedUsage[day][model]
		for _, r := range s.state.Results {
			if r.Model == model && r.Time.UTC().Format("2006-01-02") == day {
				count++
			}
		}
		if count >= cfg.DailyLimit {
			s.mu.Unlock()
			verdict = "unavailable"
			reason = "每日请求预算已耗尽"
			break
		}
		r := Result{ID: Secret()[:12], Model: model, Prompt: p.ID, PromptText: p.Text, Time: time.Now(), Verdict: "running"}
		s.state.Results = append(s.state.Results, r)
		idx := len(s.state.Results) - 1
		err := s.store.Save(s.state)
		s.mu.Unlock()
		if err != nil {
			reason = err.Error()
			break
		}
		answer, tokens, err := call(ctx, cfg, "quality-probe/"+alias+"/"+model, p.Text, func(partial string) {
			s.mu.Lock()
			s.state.Results[idx].Answer = partial
			s.mu.Unlock()
		})
		r.Answer = answer
		r.Tokens = tokens
		if err != nil {
			r.Verdict = "unavailable"
			r.Error = err.Error()
			if errors.Is(ctx.Err(), context.Canceled) {
				r.Error = "检测已取消"
			}
		} else {
			r.Verdict = Grade(p.ID, answer)
		}
		s.mu.Lock()
		s.state.Results[idx] = r
		s.store.Save(s.state)
		s.mu.Unlock()
		if r.Verdict == "passed" {
			verdict = "passed"
			break
		}
		if r.Verdict == "manual" {
			verdict = "manual"
			reason = "手动题已完成，不能作为自动放行依据"
			break
		}
		if err != nil {
			verdict = "unavailable"
			reason = r.Error
			break
		}
		verdict = "review"
		reason = "牛奶题最终答案未通过，独立复测仍异常"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, model)
	s.testCancels[model]()
	delete(s.testCancels, model)
	t := s.state.Targets[model]
	if !p.Automatic {
		s.saveLocked()
		return
	}
	// Manual hold during a run must not be overwritten by a passing probe.
	if t.State != "checking" {
		s.saveLocked()
		return
	}
	if verdict == "manual" {
		t.State = "unavailable"
	} else {
		t.State = verdict
	}
	t.Reason = reason
	if verdict == "passed" {
		t.CheckedAt = time.Now()
		t.Reason = ""
		t.RetryAfter = time.Time{}
	} else if verdict == "unavailable" {
		t.RetryAfter = time.Now().Add(15 * time.Minute)
	}
	s.saveLocked()
}
