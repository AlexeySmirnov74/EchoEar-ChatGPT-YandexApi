package ai

import "testing"

func TestManualOutputBarrierBothEventOrders(t *testing.T) {
	for _, doneFirst := range []bool{true, false} {
		b := manualOutputBarrier{responseID: "resp1"}
		sent := 0
		if doneFirst {
			if err := b.responseDone("resp1"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := b.outputDrained("resp1"); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := b.finish(func() error { sent++; return nil }); ok || err != nil || sent != 0 {
			t.Fatal(ok, err, sent)
		}
		if doneFirst {
			if err := b.outputDrained("resp1"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := b.responseDone("resp1"); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := b.finish(func() error { sent++; return nil }); !ok || err != nil || sent != 1 {
			t.Fatal(ok, err, sent)
		}
		if _, err := b.finish(func() error { sent++; return nil }); err == nil || sent != 1 {
			t.Fatal("duplicate stop accepted")
		}
	}
}
func TestManualOutputBarrierRejectsWrongGeneration(t *testing.T) {
	b := manualOutputBarrier{responseID: "current"}
	if err := b.outputDrained("old"); err == nil {
		t.Fatal("old frame completion accepted")
	}
	if err := b.responseDone("old"); err == nil {
		t.Fatal("old done accepted")
	}
}
