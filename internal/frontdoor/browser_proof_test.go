package frontdoor

import (
	"testing"
	"time"
)

func TestAdvanceBrowserProofRejectsExpiryAndLaterWithoutTouchingTimer(t *testing.T) {
	for _, offset := range []time.Duration{0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			originalDuration := 60 * time.Millisecond
			expiry := time.Now().Add(originalDuration)
			timer := time.NewTimer(originalDuration)
			defer stopTimer(timer)
			before := expiry

			if advanceBrowserProof(timer, &expiry, expiry.Add(offset), 2*time.Second, time.Now) {
				t.Fatalf("proof at %v advanced expiry", offset)
			}
			if expiry != before {
				t.Fatalf("rejected proof changed expiry: before=%v after=%v", before, expiry)
			}
			select {
			case <-timer.C:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("rejected proof postponed the original timer deadline")
			}
		})
	}
}

func TestNewBrowserProofTimerUsesRemainingDuration(t *testing.T) {
	base := time.Now()
	expiry := base.Add(400 * time.Millisecond)
	now := func() time.Time { return base.Add(300 * time.Millisecond) }
	started := time.Now()
	timer, armed := newBrowserProofTimer(expiry, now)
	if !armed {
		t.Fatal("remaining initial deadline was not armed")
	}
	defer stopTimer(timer)
	select {
	case <-timer.C:
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("initial timer used more than remaining duration: %v", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("initial timer received a fresh full interval")
	}
}

func TestAdvanceBrowserProofUsesReceiptExpiryRemainingDuration(t *testing.T) {
	base := time.Now()
	receipt := base.Add(100 * time.Millisecond)
	expiry := base.Add(time.Second)
	timeout := 400 * time.Millisecond
	now := func() time.Time { return base.Add(400 * time.Millisecond) }
	timer := time.NewTimer(time.Hour)
	defer stopTimer(timer)
	started := time.Now()

	if !advanceBrowserProof(timer, &expiry, receipt, timeout, now) {
		t.Fatal("strictly early proof with time remaining was rejected")
	}
	if want := receipt.Add(timeout); expiry != want {
		t.Fatalf("new expiry=%v want=%v", expiry, want)
	}
	select {
	case <-timer.C:
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("rearm used more than remaining duration: %v", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("rearm received the original full timeout")
	}
}

func TestBrowserProofArmRechecksAbsoluteDeadline(t *testing.T) {
	base := time.Now()
	if browserProofArmReservation <= 0 || browserProofArmReservation > 10*time.Millisecond {
		t.Fatalf("arm reservation=%v", browserProofArmReservation)
	}

	t.Run("initial_strictly_before_over_budget", func(t *testing.T) {
		calls := 0
		now := func() time.Time {
			calls++
			if calls == 1 {
				return base
			}
			return base.Add(500 * time.Millisecond)
		}
		timer, armed := newBrowserProofTimer(base.Add(time.Second), now)
		if armed || timer == nil {
			t.Fatalf("strictly-before over-budget initial arm=(timer=%v, armed=%v)", timer != nil, armed)
		}
		defer stopTimer(timer)
		if timer.Reset(time.Hour) {
			t.Fatal("rejected strictly-before initial arm left timer active")
		}
	})

	t.Run("rearm_strictly_before_over_budget", func(t *testing.T) {
		calls := 0
		now := func() time.Time {
			calls++
			if calls == 1 {
				return base
			}
			return base.Add(500 * time.Millisecond)
		}
		timer := time.NewTimer(time.Hour)
		defer stopTimer(timer)
		expiry := base.Add(2 * time.Second)
		if advanceBrowserProof(timer, &expiry, base, time.Second, now) {
			t.Fatal("strictly-before over-budget rearm succeeded")
		}
		if timer.Reset(time.Hour) {
			t.Fatal("rejected strictly-before rearm left timer active")
		}
	})

	t.Run("initial", func(t *testing.T) {
		calls := 0
		now := func() time.Time {
			calls++
			if calls == 1 {
				return base.Add(100 * time.Millisecond)
			}
			return base.Add(500 * time.Millisecond)
		}
		timer, armed := newBrowserProofTimer(base.Add(500*time.Millisecond), now)
		if armed || timer == nil {
			t.Fatalf("initial arm across expiry=(timer=%v, armed=%v)", timer != nil, armed)
		}
		defer stopTimer(timer)
		if timer.Reset(time.Hour) {
			t.Fatal("failed initial post-arm check left timer active")
		}
	})

	t.Run("rearm", func(t *testing.T) {
		calls := 0
		now := func() time.Time {
			calls++
			if calls == 1 {
				return base.Add(100 * time.Millisecond)
			}
			return base.Add(500 * time.Millisecond)
		}
		timer := time.NewTimer(time.Hour)
		defer stopTimer(timer)
		expiry := base.Add(2 * time.Second)
		if advanceBrowserProof(timer, &expiry, base, 500*time.Millisecond, now) {
			t.Fatal("rearm crossing the new absolute expiry succeeded")
		}
		if timer.Reset(time.Hour) {
			t.Fatal("failed rearm post-arm check left timer active")
		}
	})
}
