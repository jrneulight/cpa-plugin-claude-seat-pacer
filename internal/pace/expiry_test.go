package pace

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// expiryConfig is the default config with the expiry rule on or off.
func expiryConfig(enabled bool) model.Config {
	cfg := model.Defaults()
	cfg.Pace.Expiry.Enabled = enabled
	return cfg
}

// The two seats the rule exists for: A has 11% of its week left and resets in
// 21 hours, B has 95% left and resets in 5.4 days. On cost alone B wins every
// new conversation and A's last 11% lapses.
func nearReset(now time.Time) model.AuthSnapshot {
	return seat("a", now, window(model.WindowWeekly, "", 0.89, now.Add(21*time.Hour)))
}

func farFromReset(now time.Time) model.AuthSnapshot {
	return seat("b", now, window(model.WindowWeekly, "", 0.05, now.Add(time.Duration(5.4*24*float64(time.Hour)))))
}

func TestExpiryRuleRanksTheNearResetSeatFirst(t *testing.T) {
	now := testNow
	rejectedSession := window(model.WindowSession, "", 0.3, now.Add(2*time.Hour))
	rejectedSession.Status = model.StatusRejected

	cases := []struct {
		name     string
		mutate   func(*model.Config)
		snaps    []model.AuthSnapshot
		modelID  string
		want     []string
		expiring []string
	}{
		{
			name:  "off keeps the cost order",
			snaps: []model.AuthSnapshot{nearReset(now), farFromReset(now)},
			want:  []string{"b", "a"},
		},
		{
			name:     "on puts the near-reset seat first",
			mutate:   func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps:    []model.AuthSnapshot{nearReset(now), farFromReset(now)},
			want:     []string{"a", "b"},
			expiring: []string{"a"},
		},
		{
			name: "a reset outside the window keeps the cost order",
			mutate: func(c *model.Config) {
				c.Pace.Expiry.Enabled = true
				c.Pace.Expiry.Window = 12 * time.Hour
			},
			snaps: []model.AuthSnapshot{nearReset(now), farFromReset(now)},
			want:  []string{"b", "a"},
		},
		{
			name: "headroom below the minimum keeps the cost order",
			mutate: func(c *model.Config) {
				c.Pace.Expiry.Enabled = true
				c.Pace.Expiry.MinHeadroom = 0.2
			},
			snaps: []model.AuthSnapshot{nearReset(now), farFromReset(now)},
			want:  []string{"b", "a"},
		},
		{
			// C resets sooner than A and costs more, so only the reset
			// order puts it first.
			name:   "two expiring seats go soonest reset first",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				nearReset(now), farFromReset(now),
				seat("c", now, window(model.WindowWeekly, "", 0.95, now.Add(10*time.Hour))),
			},
			want:     []string{"c", "a", "b"},
			expiring: []string{"a", "c"},
		},
		{
			name:   "a spent seat is not picked",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now, window(model.WindowWeekly, "", 1, now.Add(21*time.Hour))),
				farFromReset(now),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "a refused seat is not picked",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now, window(model.WindowWeekly, "", 0.89, now.Add(21*time.Hour)), rejectedSession),
				farFromReset(now),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "a seat with no weekly window is not expiring",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now, window(model.WindowSession, "", 0.1, now.Add(time.Hour))),
				farFromReset(now),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "a weekly window with no open period is not expiring",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now, unopened(model.WindowWeekly, "", 0.89)),
				farFromReset(now),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "a weekly window already past its reset is not expiring",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now, window(model.WindowWeekly, "", 0.89, now.Add(-time.Minute))),
				farFromReset(now),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "a nearly spent family cap on the request blocks it",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now,
					window(model.WindowWeekly, "", 0.89, now.Add(21*time.Hour)),
					window(model.WindowWeeklyScoped, model.FamilyFable, 0.99, now.Add(3*24*time.Hour))),
				farFromReset(now),
			},
			modelID: fable,
			want:    []string{"b", "a"},
		},
		{
			name:   "another family's nearly spent cap does not",
			mutate: func(c *model.Config) { c.Pace.Expiry.Enabled = true },
			snaps: []model.AuthSnapshot{
				seat("a", now,
					window(model.WindowWeekly, "", 0.89, now.Add(21*time.Hour)),
					window(model.WindowWeeklyScoped, model.FamilyFable, 0.99, now.Add(3*24*time.Hour))),
				farFromReset(now),
			},
			modelID:  opus,
			want:     []string{"a", "b"},
			expiring: []string{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := expiryConfig(false)
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			modelID := tc.modelID
			if modelID == "" {
				modelID = opus
			}
			ids := make([]string, 0, len(tc.snaps))
			for _, s := range tc.snaps {
				ids = append(ids, s.AuthID)
			}
			scores := RankWithStaleness(cfg, snapshots(tc.snaps...), ids, modelID, now)
			if got := rankedIDs(scores); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rank = %v, want %v", got, tc.want)
			}
			if best, ok := Best(scores); !ok || best.AuthID != tc.want[0] {
				t.Errorf("Best = %q (%v), want %q, the head of the rank", best.AuthID, ok, tc.want[0])
			}
			var expiring []string
			for _, s := range scores {
				if s.Expiring {
					expiring = append(expiring, s.AuthID)
					if !s.Eligible {
						t.Errorf("%s is expiring and ineligible", s.AuthID)
					}
				}
			}
			slices.Sort(expiring)
			if !slices.Equal(expiring, tc.expiring) {
				t.Errorf("expiring = %v, want %v", expiring, tc.expiring)
			}
		})
	}
}

// A stale reading is not scored at all, so the expiry rule never sees it.
func TestExpiryRuleSkipsAStaleSeat(t *testing.T) {
	cfg := expiryConfig(true)
	a := nearReset(testNow)
	a.ObservedAt = testNow.Add(-time.Hour)
	scores := RankWithStaleness(cfg, snapshots(a, farFromReset(testNow)), []string{"a", "b"}, opus, testNow)
	if got := rankedIDs(scores); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("rank = %v, want b ahead of the stale a", got)
	}
	if s := scoreOf(t, scores, "a"); s.Expiring || s.Reason != model.ReasonStale {
		t.Errorf("stale a = expiring %v reason %q", s.Expiring, s.Reason)
	}
	if s := ScoreWithStaleness(cfg, a, true, "a", opus, testNow); s.Expiring {
		t.Error("ScoreWithStaleness marked a stale seat expiring")
	}
}

// Off, the rule leaves every score exactly as it was, so a status payload
// carries no expiring field.
func TestExpiryRuleOffChangesNoScore(t *testing.T) {
	snaps := snapshots(nearReset(testNow), farFromReset(testNow))
	off := Rank(expiryConfig(false).Pace, snaps, []string{"a", "b"}, opus, testNow)
	for _, s := range off {
		if s.Expiring {
			t.Errorf("%s is expiring with the rule off", s.AuthID)
		}
	}
	on := Rank(expiryConfig(true).Pace, snaps, []string{"a", "b"}, opus, testNow)
	for _, s := range on {
		s.Expiring = false
		if !reflect.DeepEqual(s, scoreOf(t, off, s.AuthID)) {
			t.Errorf("%s scores differently beyond Expiring: %+v vs %+v", s.AuthID, s, scoreOf(t, off, s.AuthID))
		}
	}
}

// Whether a binding moves is judged against the cheapest seat, so an
// expiring challenger moves nothing the cost order would not.
func TestCheapestIgnoresTheExpiryRule(t *testing.T) {
	cfg := expiryConfig(true)
	scores := Rank(cfg.Pace, snapshots(nearReset(testNow), farFromReset(testNow)), []string{"a", "b"}, opus, testNow)
	best, _ := Best(scores)
	cheapest, ok := Cheapest(scores)
	if best.AuthID != "a" || !ok || cheapest.AuthID != "b" {
		t.Fatalf("Best = %q, Cheapest = %q, want a and b", best.AuthID, cheapest.AuthID)
	}
	// B bound, A newly expiring: A costs more than B, so B keeps the binding.
	if ShouldSwitch(cfg.Pace, scoreOf(t, scores, "b"), best) {
		t.Error("an expiring challenger with a higher cost moved the binding")
	}
}
