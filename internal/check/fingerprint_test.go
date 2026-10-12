package check

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

type goldenCase struct {
	ID     string `json:"id"`
	Outputs []struct {
		Text          string `json:"text"`
		ExpectedCount int    `json:"expected_count"`
	} `json:"outputs"`
	JS struct {
		Error       string  `json:"error"`
		Prediction  string  `json:"prediction"`
		Probability float64 `json:"probability"`
		UsedOutputs int     `json:"used_outputs"`
		Parsed      []int   `json:"parsed"`
		Top         []struct {
			Model       string  `json:"model"`
			Probability float64 `json:"probability"`
			Score       float64 `json:"score"`
		} `json:"top"`
	} `json:"js"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/fingerprint_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []goldenCase `json:"cases"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil || len(doc.Cases) < 10 {
		t.Fatal("golden cases unreadable", err, len(doc.Cases))
	}
	return doc.Cases
}

func (c goldenCase) outputs() []FingerprintOutput {
	out := make([]FingerprintOutput, len(c.Outputs))
	for i, o := range c.Outputs {
		out[i] = FingerprintOutput{Text: o.Text, ExpectedCount: o.ExpectedCount}
	}
	return out
}

// The Go scorer must reproduce the vendored browser algorithm on deterministic
// synthetic answers. No archived probe responses are needed for this test.
func TestFingerprintMatchesVendoredJSOnSyntheticAnswers(t *testing.T) {
	predictions := map[string]bool{}
	for _, c := range loadGolden(t) {
		report, err := AnalyzeFingerprint(c.outputs())
		if c.JS.Error != "" {
			if err == nil {
				t.Fatalf("%s: JS rejects, Go accepted", c.ID)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		if report.Prediction != c.JS.Prediction || report.UsedOutputs != c.JS.UsedOutputs {
			t.Fatalf("%s: Go %s/%d, JS %s/%d", c.ID, report.Prediction, report.UsedOutputs, c.JS.Prediction, c.JS.UsedOutputs)
		}
		if math.Abs(report.Probability-c.JS.Probability) > 1e-9 {
			t.Fatalf("%s: probability %v vs %v", c.ID, report.Probability, c.JS.Probability)
		}
		for i, want := range c.JS.Top {
			got := report.Top[i]
			if got.Model != want.Model || math.Abs(got.Probability-want.Probability) > 1e-9 || math.Abs(got.Score-want.Score) > 1e-9 {
				t.Fatalf("%s: rank %d differs: %+v vs %+v", c.ID, i, got, want)
			}
		}
		for i, n := range c.JS.Parsed {
			if report.Parsed[i] != n {
				t.Fatalf("%s: parsed %v vs %v", c.ID, report.Parsed, c.JS.Parsed)
			}
		}
		predictions[report.Prediction] = true
	}
	if len(predictions) < 2 {
		t.Fatalf("synthetic cases exercise only %d prediction", len(predictions))
	}
}

func TestFingerprintBankIsConsistent(t *testing.T) {
	bank, err := loadFingerprintBank()
	if err != nil {
		t.Fatal(err)
	}
	if !HasFingerprintModel("gpt-6-astra") || HasFingerprintModel("nope") {
		t.Fatal("bank candidate lookup wrong")
	}
	if len(bank.Models) != len(bank.Robust.Hellinger.Centroids) {
		t.Fatal("candidate and centroid counts differ")
	}
	for in, want := range map[string]string{
		"magpie-252-ca704414/codex/gpt-6-astra": "gpt-6-astra",
		"gpt-5.5":                               "gpt-5.5",
		"a/b/not-a-candidate":                   "",
	} {
		if got := fingerprintModelFor(in, ""); got != want {
			t.Fatalf("%s → %q, want %q", in, got, want)
		}
	}
	if fingerprintModelFor("x/y", "gpt-5.5") != "gpt-5.5" || fingerprintModelFor("x/gpt-5.5", "ghost") != "" {
		t.Fatal("override not honoured")
	}
}

func TestFingerprintRejectsShortAndEmptyAnswers(t *testing.T) {
	if _, err := AnalyzeFingerprint([]FingerprintOutput{{Text: "我无法完成这个任务", ExpectedCount: 300}}); err == nil {
		t.Fatal("refusal scored")
	}
	short := strings.TrimSuffix(strings.Repeat("7, ", 100), ", ")
	if _, err := AnalyzeFingerprint([]FingerprintOutput{{Text: short, ExpectedCount: 300}}); err == nil {
		t.Fatal("answer under 55% of the requested count scored")
	}
	if got := parseFingerprintNumbers("1, 2, 3 abc 4, 5, 6, 7 999 0 356 8"); len(got) != 5 {
		t.Fatalf("longest run wrong: %v", got)
	}
}
