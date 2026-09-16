package rehome

import (
	"testing"
	"time"
)

func TestTranscriptFloor(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		days int
		want time.Duration
		ok   bool
	}{
		{30, 15 * day, true},
		{8, 4 * day, true},
		{7, 84 * time.Hour, true},
		{1, 12 * time.Hour, true},
		{0, 0, false},
		{-3, 0, false},
	}
	for _, tc := range cases {
		floor, ok := TranscriptFloor(MtimePolicy{CleanupPeriodDays: tc.days, Now: fixedNow})
		if ok != tc.ok {
			t.Errorf("days=%d ok=%v, want %v", tc.days, ok, tc.ok)
			continue
		}
		if ok && !floor.Equal(fixedNow.Add(-tc.want)) {
			t.Errorf("days=%d floor=%v, want now-%v", tc.days, floor, tc.want)
		}
	}
	if floor, ok := TranscriptFloor(MtimePolicy{CleanupPeriodDays: 30}); !ok || time.Since(floor) < 14*day {
		t.Errorf("zero Now must mean time.Now(): floor=%v ok=%v", floor, ok)
	}
}

func TestTranscriptMtime(t *testing.T) {
	day := 24 * time.Hour
	p := MtimePolicy{CleanupPeriodDays: 30, Now: fixedNow}
	if got, raised := transcriptMtime(fixedNow.Add(-10*day), p); raised || !got.Equal(fixedNow.Add(-10*day)) {
		t.Errorf("recent: %v raised=%v", got, raised)
	}
	if got, raised := transcriptMtime(fixedNow.Add(-45*day), p); !raised || !got.Equal(fixedNow.Add(-15*day)) {
		t.Errorf("old: %v raised=%v", got, raised)
	}
	if got, raised := transcriptMtime(time.Time{}, p); raised || !got.Equal(fixedNow) {
		t.Errorf("zero orig: %v raised=%v", got, raised)
	}
	if got, raised := transcriptMtime(fixedNow.Add(-45*day), MtimePolicy{Now: fixedNow}); raised || !got.Equal(fixedNow.Add(-45*day)) {
		t.Errorf("never sweep: %v raised=%v", got, raised)
	}
	src := fixedNow.Add(-40 * day)
	if got := artifactMtime(src, p); !got.Equal(fixedNow) {
		t.Errorf("artifact refresh = %v", got)
	}
	p.PreserveSidecars = true
	if got := artifactMtime(src, p); !got.Equal(src) {
		t.Errorf("artifact preserve = %v", got)
	}
}
