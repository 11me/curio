package curio

import (
	"math"
	"sort"
)

// Answer is the model's answer to one criterion for one story.
type Answer struct {
	Criterion string  `json:"criterion"`
	Answer    float64 `json:"answer"` // 0 to 1, how likely the answer is yes
	Note      string  `json:"note"`   // short reason in plain words
}

// Reason is one row of the "why it fits" data.
type Reason struct {
	Criterion    string  `json:"criterion"`
	Label        string  `json:"label"`
	Question     string  `json:"question"`
	Answer       float64 `json:"answer"`
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"` // weight * answer
	Note         string  `json:"note"`
}

// Fit computes the 0 to 1 fit and the reason rows, in criteria order.
// Answers for unknown criteria are ignored; missing answers count as 0.
func Fit(bias float64, criteria []Criterion, answers []Answer) (float64, []Reason) {
	byID := make(map[string]Answer, len(answers))
	for _, a := range answers {
		byID[a.Criterion] = a
	}
	z := bias
	rows := make([]Reason, 0, len(criteria))
	for _, cr := range criteria {
		a := byID[cr.ID]
		p := clamp01(a.Answer)
		c := cr.Weight * p
		z += c
		rows = append(rows, Reason{
			Criterion: cr.ID, Label: cr.Label, Question: cr.Question,
			Answer: round(p, 2), Weight: cr.Weight, Contribution: round(c, 3), Note: a.Note,
		})
	}
	return round(1/(1+math.Exp(-z)), 3), rows
}

// TopReasons picks up to n rows that explain the fit best: the largest
// positive contributions for a story above the cutoff, otherwise the rows
// with the largest effect either way. Rows with almost no effect are skipped.
func TopReasons(rows []Reason, fit, cutoff float64, n int) []Reason {
	picked := make([]Reason, 0, len(rows))
	for _, r := range rows {
		if math.Abs(r.Contribution) < 0.15 {
			continue
		}
		if fit >= cutoff && r.Contribution < 0 {
			continue
		}
		picked = append(picked, r)
	}
	sort.SliceStable(picked, func(i, j int) bool {
		return math.Abs(picked[i].Contribution) > math.Abs(picked[j].Contribution)
	})
	if len(picked) > n {
		picked = picked[:n]
	}
	return picked
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
