package curio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testCriteria = []Criterion{
	{ID: "picks", Label: "Like your picks", Question: "q1", Weight: 2},
	{ID: "release", Label: "A release", Question: "q2", Weight: 1},
	{ID: "drama", Label: "Drama", Question: "q3", Weight: -1.5},
}

func TestFit(t *testing.T) {
	tests := []struct {
		name    string
		answers []Answer
		want    float64
	}{
		{name: "no answers gives sigmoid of bias", answers: nil, want: 0.119},
		{name: "all yes", answers: []Answer{{"picks", 1, ""}, {"release", 1, ""}, {"drama", 1, ""}}, want: 0.378},
		{name: "positive only", answers: []Answer{{"picks", 1, ""}, {"release", 1, ""}}, want: 0.731},
		{name: "values are clamped", answers: []Answer{{"picks", 5, ""}, {"drama", -3, ""}}, want: 0.5},
		{name: "unknown criteria are ignored", answers: []Answer{{"other", 1, ""}}, want: 0.119},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fit, rows := Fit(-2, testCriteria, tt.answers)
			assert.InDelta(t, tt.want, fit, 0.001)
			require.Len(t, rows, 3)
			assert.Equal(t, "picks", rows[0].Criterion)
		})
	}
}

func TestTopReasons(t *testing.T) {
	answers := []Answer{{"picks", 0.9, "close to your Rust picks"}, {"release", 0.5, "a release"}, {"drama", 0.3, "some drama"}}
	fit, rows := Fit(-1, testCriteria, answers)

	top := TopReasons(rows, fit, 0.5, 3)
	require.Len(t, top, 2, "negative rows are hidden for stories above the cutoff")
	assert.Equal(t, "picks", top[0].Criterion)
	assert.Equal(t, "close to your Rust picks", top[0].Note)
	assert.Equal(t, "release", top[1].Criterion)

	low := TopReasons(rows, fit, 0.95, 1)
	require.Len(t, low, 1)
	assert.Equal(t, "picks", low[0].Criterion)
}
