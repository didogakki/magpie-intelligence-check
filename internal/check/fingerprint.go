package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Backend port of web/modeltrace/fingerprint-core.js (ModelTrace's unified global
// robust digit fingerprint). The scheduler must not depend on a browser being open,
// so scoring runs here against the same embedded bank. fingerprint_test.go compares
// it with the vendored JS on archived answers. Arithmetic is kept in the same order as
// the JS; float64(a*b) stops the compiler from fusing multiply-adds.
const (
	fpValueMin  = 1
	fpValueMax  = 355
	fpDimension = fpValueMax - fpValueMin + 1
	fpAlpha     = 0.5
	// A trace batch has three answers; the bank is calibrated for up to three.
	fpMaxQueries = 3
)

type fpBank struct {
	Models []struct {
		ID          string    `json:"id"`
		DisplayName string    `json:"display_name"`
		Family      string    `json:"family"`
		Counts      []float64 `json:"counts"`
	} `json:"models"`
	Robust struct {
		ModelOrder []string `json:"model_order"`
		Hellinger  struct {
			FeatureMean   []float64   `json:"feature_mean"`
			FeatureScale  []float64   `json:"feature_scale"`
			NuisanceBasis [][]float64 `json:"nuisance_basis"`
			Centroids     [][]float64 `json:"centroids"`
		} `json:"hellinger"`
		OrderedBlocks *struct {
			Weight               float64       `json:"weight"`
			FeatureMean          []float64     `json:"feature_mean"`
			FeatureScale         []float64     `json:"feature_scale"`
			NuisanceBasis        [][]float64   `json:"nuisance_basis"`
			Centroids            [][]float64   `json:"centroids"`
			EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
		} `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta float64 `json:"beta"`
	} `json:"calibration"`
}

var (
	fpOnce    sync.Once
	fpLoaded  *fpBank
	fpLoadErr error
)

// loadFingerprintBank parses the embedded bank once.
func loadFingerprintBank() (*fpBank, error) {
	fpOnce.Do(func() {
		raw, err := web.ReadFile("web/modeltrace/unified_bank.json")
		if err != nil {
			fpLoadErr = err
			return
		}
		var bank fpBank
		if err = json.Unmarshal(raw, &bank); err != nil {
			fpLoadErr = err
			return
		}
		fpLoadErr = bank.validate()
		if fpLoadErr == nil {
			fpLoaded = &bank
		}
	})
	return fpLoaded, fpLoadErr
}

func (b *fpBank) validate() error {
	n := len(b.Models)
	h := b.Robust.Hellinger
	if n == 0 || len(h.Centroids) != n || len(h.FeatureMean) != fpDimension || len(h.FeatureScale) != fpDimension {
		return errors.New("指纹库结构无效")
	}
	for i, m := range b.Models {
		if len(m.Counts) != fpDimension {
			return fmt.Errorf("指纹库候选 %s 计数无效", m.ID)
		}
		if i < len(b.Robust.ModelOrder) && b.Robust.ModelOrder[i] != m.ID {
			return errors.New("指纹库候选顺序与质心不一致")
		}
	}
	if o := b.Robust.OrderedBlocks; o != nil && o.Weight != 0 && (len(o.Centroids) != n || len(o.FeatureMean) != 74 || len(o.FeatureScale) != 74) {
		return errors.New("指纹库有序块结构无效")
	}
	for k := 1; k <= fpMaxQueries; k++ {
		if b.Calibration[strconv.Itoa(k)].Beta == 0 {
			return errors.New("指纹库缺少校准参数")
		}
	}
	return nil
}

// HasFingerprintModel reports whether id is a candidate in the embedded bank.
func HasFingerprintModel(id string) bool {
	bank, err := loadFingerprintBank()
	if err != nil {
		return false
	}
	for _, m := range bank.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

var fpDigits = regexp.MustCompile(`\d+`)

// parseFingerprintNumbers mirrors parseNumbers: the longest run of integers in 1–355,
// where a letter between two numbers starts a new run.
func parseFingerprintNumbers(text string) []int {
	var best, current []int
	previousEnd := 0
	for _, m := range fpDigits.FindAllStringIndex(text, -1) {
		if len(current) > 0 && strings.ContainsFunc(text[previousEnd:m[0]], unicode.IsLetter) {
			if len(current) > len(best) {
				best = current
			}
			current = nil
		}
		if v, err := strconv.ParseFloat(text[m[0]:m[1]], 64); err == nil && v >= fpValueMin && v <= fpValueMax {
			current = append(current, int(v))
		}
		previousEnd = m[1]
	}
	if len(current) > len(best) {
		best = current
	}
	return best
}

func fpCount(numbers []int) []float64 {
	counts := make([]float64, fpDimension)
	for _, n := range numbers {
		counts[n-fpValueMin]++
	}
	return counts
}

func fpMean(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}

func fpStandardize(values []float64) []float64 {
	center := fpMean(values)
	sq := make([]float64, len(values))
	for i, v := range values {
		d := v - center
		sq[i] = float64(d * d)
	}
	scale := math.Max(math.Sqrt(fpMean(sq)), 1e-12)
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = (v - center) / scale
	}
	return out
}

func fpDot(a, b []float64) float64 {
	total := 0.0
	for i := range a {
		total += float64(a[i] * b[i])
	}
	return total
}

func fpNormalized(values []float64) []float64 {
	scale := math.Max(math.Sqrt(fpDot(values, values)), 1e-12)
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = v / scale
	}
	return out
}

func fpSubtractBasis(values []float64, basis [][]float64) []float64 {
	out := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := fpDot(out, vector)
		for i := range out {
			out[i] -= float64(projection * vector[i])
		}
	}
	return out
}

func fpRobustCounts(counts []float64, bank *fpBank) []float64 {
	a := bank.Robust.Hellinger
	total := fpSum(counts) + fpAlpha*fpDimension
	projected := make([]float64, fpDimension)
	for i, v := range counts {
		projected[i] = (math.Sqrt((v+fpAlpha)/total) - a.FeatureMean[i]) / a.FeatureScale[i]
	}
	projected = fpNormalized(fpSubtractBasis(projected, a.NuisanceBasis))
	scores := make([]float64, len(a.Centroids))
	for i, c := range a.Centroids {
		scores[i] = fpDot(projected, c)
	}
	return fpStandardize(scores)
}

func fpSum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func fpOrderedFeature(numbers []int) []float64 {
	var pieces []float64
	base, remainder := len(numbers)/4, len(numbers)%4
	start := 0
	for i := 0; i < 4; i++ {
		size := base
		if i < remainder {
			size++
		}
		bins := make([]float64, 16)
		for j := range bins {
			bins[j] = 0.5
		}
		for _, v := range numbers[start : start+size] {
			bins[min(15, int(math.Floor(float64(v-1)/355*16)))]++
		}
		start += size
		total := fpSum(bins)
		for _, v := range bins {
			pieces = append(pieces, math.Sqrt(v/total))
		}
	}
	last := make([]float64, 10)
	for j := range last {
		last[j] = 0.5
	}
	for _, v := range numbers {
		last[v%10]++
	}
	total := fpSum(last)
	for _, v := range last {
		pieces = append(pieces, math.Sqrt(v/total))
	}
	return pieces
}

func fpOrderedScores(numbers []int, bank *fpBank) []float64 {
	a := bank.Robust.OrderedBlocks
	feature := fpOrderedFeature(numbers)
	std := make([]float64, len(feature))
	for i, v := range feature {
		std[i] = (v - a.FeatureMean[i]) / a.FeatureScale[i]
	}
	unit := fpNormalized(std)
	template := make([]float64, len(a.Centroids))
	for m := range a.Centroids {
		best := math.Inf(-1)
		for _, env := range a.EnvironmentCentroids {
			best = math.Max(best, fpDot(unit, env[m]))
		}
		template[m] = best
	}
	template = fpStandardize(template)
	projected := fpNormalized(fpSubtractBasis(std, a.NuisanceBasis))
	nuisance := make([]float64, len(a.Centroids))
	for i, c := range a.Centroids {
		nuisance[i] = fpDot(projected, c)
	}
	nuisance = fpStandardize(nuisance)
	mixed := make([]float64, len(template))
	for i := range template {
		mixed[i] = 0.5*template[i] + 0.5*nuisance[i]
	}
	return fpStandardize(mixed)
}

func fpRobustNumbers(numbers []int, bank *fpBank) []float64 {
	marginal := fpRobustCounts(fpCount(numbers), bank)
	a := bank.Robust.OrderedBlocks
	if a == nil || a.Weight == 0 {
		return marginal
	}
	ordered := fpOrderedScores(numbers, bank)
	out := make([]float64, len(marginal))
	for i := range marginal {
		out[i] = (1-a.Weight)*marginal[i] + a.Weight*ordered[i]
	}
	return out
}

func fpSoftmax(values []float64) []float64 {
	maximum := math.Inf(-1)
	for _, v := range values {
		maximum = math.Max(maximum, v)
	}
	weights := make([]float64, len(values))
	for i, v := range values {
		weights[i] = math.Exp(v - maximum)
	}
	total := fpSum(weights)
	for i := range weights {
		weights[i] /= total
	}
	return weights
}

// FingerprintCandidate is one closed-set candidate of a report.
type FingerprintCandidate struct {
	Model       string  `json:"model"`
	Probability float64 `json:"probability"`
	Score       float64 `json:"score"`
}

// FingerprintReport is the closed-set result for one batch. It says which bank
// candidate the three answers resemble most; it is not proof of a physical model.
type FingerprintReport struct {
	Prediction  string                 `json:"prediction"`
	Probability float64                `json:"probability"`
	UsedOutputs int                    `json:"used_outputs"`
	Parsed      []int                  `json:"parsed"`
	Top         []FingerprintCandidate `json:"top"`
}

// FingerprintOutput is one answer with the count it was asked for.
type FingerprintOutput struct {
	Text          string
	ExpectedCount int
}

// AnalyzeFingerprint scores answers like analyzeGlobalOutputs. Answers with fewer than
// max(80, ceil(0.55*expected)) numbers are not used. It errors when none is usable.
func AnalyzeFingerprint(outputs []FingerprintOutput) (FingerprintReport, error) {
	bank, err := loadFingerprintBank()
	if err != nil {
		return FingerprintReport{}, err
	}
	var scores [][]float64
	parsed := make([]int, len(outputs))
	for i, out := range outputs {
		numbers := parseFingerprintNumbers(out.Text)
		parsed[i] = len(numbers)
		minimum := 80
		if out.ExpectedCount > 0 {
			minimum = max(80, int(math.Ceil(float64(out.ExpectedCount)*0.55)))
		}
		if len(numbers) >= minimum {
			scores = append(scores, fpRobustNumbers(numbers, bank))
		}
	}
	if len(scores) == 0 {
		return FingerprintReport{Parsed: parsed}, errors.New("没有可用回答")
	}
	combined := make([]float64, len(bank.Models))
	for m := range combined {
		total := 0.0
		for _, s := range scores {
			total += s[m]
		}
		combined[m] = total / float64(len(scores))
	}
	beta := bank.Calibration[strconv.Itoa(min(len(scores), fpMaxQueries))].Beta
	scaled := make([]float64, len(combined))
	for i, v := range combined {
		scaled[i] = beta * v
	}
	probabilities := fpSoftmax(scaled)
	top := make([]FingerprintCandidate, len(bank.Models))
	for i, m := range bank.Models {
		top[i] = FingerprintCandidate{Model: m.ID, Probability: probabilities[i], Score: combined[i]}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Probability > top[j].Probability })
	return FingerprintReport{Prediction: top[0].Model, Probability: top[0].Probability, UsedOutputs: len(scores), Parsed: parsed, Top: top}, nil
}

// fingerprintModelFor derives the bank candidate a scheduled model should resemble:
// an explicit override, otherwise the last path segment of the model ID
// (magpie-252-xxxx/codex/gpt-6-astra → gpt-6-astra). "" if the bank has no such candidate.
func fingerprintModelFor(model, override string) string {
	candidate := override
	if candidate == "" {
		candidate = model[strings.LastIndex(model, "/")+1:]
	}
	if HasFingerprintModel(candidate) {
		return candidate
	}
	return ""
}
