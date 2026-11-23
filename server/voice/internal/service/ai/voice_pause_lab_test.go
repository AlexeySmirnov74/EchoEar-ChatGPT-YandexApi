package ai

import "testing"

// Synthetic energy frames demonstrate the algorithm's boundary, not the
// distribution of actual speech pauses or the cause of a reported failure.
func TestVoicePauseLab600Splits720msPause(t *testing.T) {
	for _, tc := range []struct{ silenceMS, wantBeforeResumption int }{{600, 1}, {900, 0}, {1200, 0}} {
		v, err := newStreamLocalVAD(400, 120, tc.silenceMS, 300, 30000)
		if err != nil {
			t.Fatal(err)
		}
		commits := 0
		feed := func(value int16) {
			e, err := v.feed(pcmFrame(value))
			if err != nil {
				t.Fatal(err)
			}
			if e.commit {
				commits++
			}
		}
		for i := 0; i < 3; i++ {
			feed(1500)
		}
		for i := 0; i < 12; i++ {
			feed(0)
		} // 720 ms
		if commits != tc.wantBeforeResumption {
			t.Fatalf("silence %d: got %d", tc.silenceMS, commits)
		}
		for i := 0; i < 3; i++ {
			feed(1500)
		}
		for i := 0; i < 25; i++ {
			feed(0)
		}
		if commits != tc.wantBeforeResumption+1 {
			t.Fatalf("silence %d: final commits %d", tc.silenceMS, commits)
		}
	}
}
