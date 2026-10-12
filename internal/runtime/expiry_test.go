package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
)

const expiryOn = testConfigYAML + "pace.expiry.enabled: true\n"

const opusModel = "claude-opus-5"

// expirySeats puts the rule's two seats in the store: near-reset has 11% of
// its week left and resets in 21 hours, far-reset has 95% left and resets in
// 5.4 days. Cost alone picks far-reset.
func (tp *testPlugin) expirySeats() {
	tp.quota.Put(seat("near-reset", testNow,
		model.Window{Kind: model.WindowWeekly, Utilization: 0.89, ResetsAt: testNow.Add(21 * time.Hour), Duration: model.WeeklyDuration}))
	tp.quota.Put(seat("far-reset", testNow,
		model.Window{Kind: model.WindowWeekly, Utilization: 0.05, ResetsAt: testNow.Add(time.Duration(5.4 * 24 * float64(time.Hour))), Duration: model.WeeklyDuration}))
}

func TestColdPickFollowsTheExpiryRule(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"off", testConfigYAML, "far-reset"},
		{"on", expiryOn, "near-reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, tc.config)
			tp.expirySeats()
			resp := tp.pick(t, pickRequest(opusModel, "k", "near-reset", "far-reset"))
			if !resp.Handled || resp.AuthID != tc.want {
				t.Fatalf("response = %+v, want %s", resp, tc.want)
			}
			d := tp.lastDecision(t)
			if len(d.Scores) == 0 || d.Scores[0].AuthID != tc.want {
				t.Errorf("decision scores lead with %+v, want %s", d.Scores, tc.want)
			}

			// The status view's next pick is the pick the scheduler made.
			st := tp.Status(testNow, opusModel)
			scores := make([]model.Score, 0, len(st.Auths))
			for _, row := range st.Auths {
				scores = append(scores, row.Score)
			}
			if best, ok := pace.Best(scores); !ok || best.AuthID != tc.want {
				t.Errorf("status next pick = %q, want %q", best.AuthID, tc.want)
			}
			for _, row := range st.Auths {
				wantExpiring := tc.name == "on" && row.AuthID == "near-reset"
				if row.Score.Expiring != wantExpiring {
					t.Errorf("%s expiring = %v, want %v", row.AuthID, row.Score.Expiring, wantExpiring)
				}
			}
			if st.Config.Pace.Expiry.Enabled != (tc.name == "on") || st.Config.Pace.Expiry.Window != 24*time.Hour {
				t.Errorf("status config expiry = %+v", st.Config.Pace.Expiry)
			}
		})
	}
}

// A conversation bound to a seat stays there when another seat becomes
// expiring, whether or not affinity overrides pace: the expiring seat costs
// more, so the hysteresis test that would move the binding today does not
// fire.
func TestABindingIsNotMovedByANewlyExpiringSeat(t *testing.T) {
	for _, config := range []string{expiryOn, expiryOn + "affinity:\n  override-threshold: false\n"} {
		tp := newTestPlugin(t, config)
		tp.expirySeats()
		tp.bindings.Bind("claude", opusModel, "k", "far-reset", testNow.Add(-time.Minute))

		resp := tp.pick(t, pickRequest(opusModel, "k", "near-reset", "far-reset"))
		if resp.AuthID != "far-reset" {
			t.Fatalf("config %q: response = %+v, want the bound far-reset", config, resp)
		}
		if d := tp.lastDecision(t); d.Kind != model.DecisionAffinityHit {
			t.Errorf("config %q: decision = %+v, want an affinity hit", config, d)
		}
	}
}

// A binding moves exactly when it would move today, and lands where the
// expiry order says. The bound seat costs about -0.10: the expiring seat, at
// about -0.07, would not beat it by the hysteresis margin, but the cheapest
// seat, at about -0.20, does, so the binding moves as it does with the rule
// off, and the expiring seat takes the conversation.
func TestAMovingBindingLandsOnTheExpiringSeat(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"off", testConfigYAML, "far-reset"},
		{"on", expiryOn, "near-reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, tc.config+"affinity:\n  override-threshold: false\n")
			tp.expirySeats()
			tp.quota.Put(seat("mid-cost", testNow,
				model.Window{Kind: model.WindowWeekly, Utilization: 0.057, ResetsAt: testNow.Add(6 * 24 * time.Hour), Duration: model.WeeklyDuration}))
			tp.bindings.Bind("claude", opusModel, "k", "mid-cost", testNow.Add(-time.Minute))

			resp := tp.pick(t, pickRequest(opusModel, "k", "near-reset", "far-reset", "mid-cost"))
			if resp.AuthID != tc.want {
				t.Fatalf("response = %+v, want %s", resp, tc.want)
			}
			d := tp.lastDecision(t)
			if d.Kind != model.DecisionFailover || d.PreviousAuthID != "mid-cost" || !strings.Contains(d.Note, "hysteresis") {
				t.Errorf("decision = %+v", d)
			}
		})
	}
}

// A seat the host holds off is no candidate, so it is not expiring either.
func TestAHeldSeatIsNotExpiring(t *testing.T) {
	row := model.AuthStatus{HostStatus: "disabled", Score: model.Score{Eligible: true, Expiring: true}}
	ApplyHostState(&row)
	if row.Score.Eligible || row.Score.Expiring {
		t.Errorf("held seat = %+v, want ineligible and not expiring", row.Score)
	}
}
