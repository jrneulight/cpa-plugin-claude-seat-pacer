package pace

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// liveReserve is the reserve the fork exists for: 70% of the session, the
// all-models week and the Fable week.
var liveReserve = model.Ceilings{"five_hour": 0.70, "seven_day": 0.70, "fable": 0.70}

// No ceilings scores exactly as ScoreAuth does, field for field, for every
// shape of snapshot the scorer distinguishes. The printed form is compared
// because a NaN cost never equals itself.
func TestNoCeilingsScoresAsScoreAuth(t *testing.T) {
	now := testNow
	rejected := paced(model.WindowSession, "", now, 0.5, 0.4)
	rejected.Status = model.StatusRejected
	snaps := []model.AuthSnapshot{
		seat("a", now, weekly(now, 0.5, 0.3)),
		seat("b", now, weekly(now, 0.9, 0.99), paced(model.WindowSession, "", now, 0.2, 0.7)),
		seat("c", now, weekly(now, 0.5, 1.2)),
		seat("d", now, rejected, weekly(now, 0.1, 0.1)),
		seat("e", now, weekly(now, 0.5, math.NaN())),
		seat("f", now, paced(model.WindowWeeklyScoped, "Fable", now, 0.3, 0.75), weekly(now, 0.3, 0.2)),
		seat("g", now),
	}
	for _, cfg := range []model.PaceConfig{expiryConfig(false).Pace, expiryConfig(true).Pace} {
		for _, snap := range snaps {
			for _, modelID := range []string{opus, fable} {
				want := ScoreAuth(cfg, snap, modelID, now)
				for _, c := range []model.Ceilings{nil, {}} {
					if got := ScoreAuthWithin(cfg, snap, c, modelID, now); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
						t.Errorf("%s %s ceilings %v: got %+v, want %+v", snap.AuthID, modelID, c, got, want)
					}
				}
			}
		}
		ids := []string{"a", "b", "c", "d", "e", "f", "g"}
		if got, want := RankWithin(cfg, snapshots(snaps...), nil, ids, opus, now), Rank(cfg, snapshots(snaps...), ids, opus, now); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
			t.Errorf("RankWithin nil = %v, want %v", rankedIDs(got), rankedIDs(want))
		}
	}
}

// The eligibility gate: a bearing window at or past its ceiling holds the
// credential off with the reserve reason; below it, and on a window the
// request cannot reach, it does not.
func TestReserveCeilingGatesBearingWindows(t *testing.T) {
	now := testNow
	session := func(util float64) model.Window { return paced(model.WindowSession, "", now, 0.4, util) }
	week := func(util float64) model.Window { return weekly(now, 0.6, util) }
	fableWeek := func(util float64) model.Window {
		return paced(model.WindowWeeklyScoped, "Fable", now, 0.6, util)
	}
	rejected := session(0.75)
	rejected.Status = model.StatusRejected

	cases := []struct {
		name     string
		ceilings model.Ceilings
		windows  []model.Window
		modelID  string
		eligible bool
		reason   string
	}{
		{"live case: session at its ceiling", liveReserve, []model.Window{session(0.70), week(0.57)}, opus, false, model.ReasonReserve},
		{"session just under its ceiling", liveReserve, []model.Window{session(0.6999), week(0.57)}, opus, true, ""},
		{"weekly past its ceiling", liveReserve, []model.Window{session(0.1), week(0.71)}, opus, false, model.ReasonReserve},
		{"Fable ceiling blocks a Fable request", liveReserve, []model.Window{session(0.1), week(0.3), fableWeek(0.72)}, fable, false, model.ReasonReserve},
		{"Fable ceiling leaves an Opus request alone", liveReserve, []model.Window{session(0.1), week(0.3), fableWeek(0.72)}, opus, true, ""},
		{"no ceiling on the window", model.Ceilings{"seven_day": 0.7}, []model.Window{session(0.9), week(0.3)}, opus, true, ""},
		{"a full window under a ceiling reads as reserve", liveReserve, []model.Window{session(1.0), week(0.3)}, opus, false, model.ReasonReserve},
		{"an uncapped full window beside one past its ceiling reads as reserve", model.Ceilings{"seven_day": 0.7}, []model.Window{session(1.0), week(0.8)}, opus, false, model.ReasonReserve},
		{"spent where no ceiling reaches", model.Ceilings{"fable": 0.7}, []model.Window{session(1.0), week(0.3)}, opus, false, model.ReasonSpent},
		{"a refusal outranks the reserve", liveReserve, []model.Window{rejected, week(0.3)}, opus, false, model.ReasonRejected},
		{"an unreadable reading outranks the reserve", liveReserve, []model.Window{session(0.8), week(math.NaN())}, opus, false, model.ReasonBadReading},
		{"a missing reading is not gated", liveReserve, []model.Window{week(0.3)}, fable, true, ""},
		{"a ceiling of one gates at full", model.Ceilings{"five_hour": 1}, []model.Window{session(0.99), week(0.3)}, opus, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreAuthWithin(expiryConfig(false).Pace, seat("a", now, tc.windows...), tc.ceilings, tc.modelID, now)
			if s.Eligible != tc.eligible || s.Reason != tc.reason {
				t.Errorf("eligible %v reason %q, want %v %q", s.Eligible, s.Reason, tc.eligible, tc.reason)
			}
		})
	}
}

// Expiry headroom is measured to the ceiling. The live seat has 43% of its
// week unused but only 13% below its 70% ceiling.
func TestExpiryHeadroomIsMeasuredToTheCeiling(t *testing.T) {
	now := testNow
	week := window(model.WindowWeekly, "", 0.57, now.Add(20*time.Hour))
	cases := []struct {
		name     string
		headroom float64
		ceilings model.Ceilings
		session  float64
		modelID  string
		expiring bool
		eligible bool
	}{
		{"live case: session at ceiling is blocked, not expiring", 0.03, liveReserve, 0.70, opus, false, false},
		{"session clear: 13% under the ceiling is expiring", 0.03, liveReserve, 0.40, opus, true, true},
		{"min-headroom above the ceiling headroom", 0.15, liveReserve, 0.40, opus, false, true},
		{"the same seat with no reserve clears 0.15", 0.15, nil, 0.40, opus, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := expiryConfig(true).Pace
			cfg.Expiry.MinHeadroom = tc.headroom
			snap := seat("a", now, paced(model.WindowSession, "", now, 0.5, tc.session), week)
			s := ScoreAuthWithin(cfg, snap, tc.ceilings, tc.modelID, now)
			if s.Expiring != tc.expiring || s.Eligible != tc.eligible {
				t.Errorf("expiring %v eligible %v (%q), want %v %v", s.Expiring, s.Eligible, s.Reason, tc.expiring, tc.eligible)
			}
		})
	}

	// A family ceiling bearing on the request bounds headroom the same way.
	cfg := expiryConfig(true).Pace
	cfg.Expiry.MinHeadroom = 0.1
	snap := seat("a", now, week, paced(model.WindowWeeklyScoped, "Fable", now, 0.5, 0.65))
	if s := ScoreAuthWithin(cfg, snap, liveReserve, fable, now); !s.Eligible || s.Expiring {
		t.Errorf("Fable 5%% under its ceiling: %+v, want eligible and not expiring", s)
	}
	if s := ScoreAuthWithin(cfg, snap, liveReserve, opus, now); !s.Expiring {
		t.Errorf("Opus request ignores the Fable ceiling: %+v, want expiring", s)
	}
}

// Cost under a ceiling c paces utilization/c against the curve: slack is
// Target(elapsed) - utilization/c.
func TestCostPacesAgainstTheCeiling(t *testing.T) {
	now := testNow
	cfg := expiryConfig(false).Pace
	snap := seat("a", now, weekly(now, 0.5, 0.35))
	target := Target(cfg, 0.5) // 0.55 at the default landing

	plain := ScoreAuth(cfg, snap, opus, now)
	reserved := ScoreAuthWithin(cfg, snap, model.Ceilings{"seven_day": 0.7}, opus, now)

	if ws := windowScoreOf(t, plain, model.WindowWeekly, ""); math.Abs(ws.Slack-(target-0.35)) > tolerance || ws.Ceiling != 0 {
		t.Errorf("plain slack %v ceiling %v, want %v and none", ws.Slack, ws.Ceiling, target-0.35)
	}
	ws := windowScoreOf(t, reserved, model.WindowWeekly, "")
	if want := target - 0.35/0.7; math.Abs(ws.Slack-want) > tolerance || ws.Ceiling != 0.7 || ws.Utilization != 0.35 {
		t.Errorf("reserved window %+v, want slack %v, ceiling 0.7, raw utilization 0.35", ws, want)
	}
	if want := -cfg.WeeklyWeight * (target - 0.5); math.Abs(reserved.Cost-want) > tolerance {
		t.Errorf("reserved cost %v, want %v", reserved.Cost, want)
	}

	// Reserved, the seat no longer outranks one that is genuinely further
	// behind its own curve.
	other := seat("b", now, weekly(now, 0.5, 0.42))
	ceilings := map[string]model.Ceilings{"a": {"seven_day": 0.7}}
	if got := rankedIDs(Rank(cfg, snapshots(snap, other), []string{"a", "b"}, opus, now)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("without the reserve ranked %v, want a first", got)
	}
	if got := rankedIDs(RankWithin(cfg, snapshots(snap, other), ceilings, []string{"a", "b"}, opus, now)); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Errorf("with the reserve ranked %v, want b first", got)
	}
}

// A binding on a seat at its ceiling yields to any eligible challenger.
func TestShouldSwitchLeavesAReservedIncumbent(t *testing.T) {
	now := testNow
	cfg := expiryConfig(false).Pace
	incumbent := ScoreAuthWithin(cfg, seat("a", now, weekly(now, 0.5, 0.1), paced(model.WindowSession, "", now, 0.5, 0.7)), liveReserve, opus, now)
	challenger := ScoreAuth(cfg, seat("b", now, weekly(now, 0.5, 0.6)), opus, now)
	if incumbent.Eligible || incumbent.Reason != model.ReasonReserve {
		t.Fatalf("incumbent = %+v, want reserve", incumbent)
	}
	if !ShouldSwitch(cfg, incumbent, challenger) {
		t.Error("a reserved incumbent did not yield to an eligible challenger")
	}
}
