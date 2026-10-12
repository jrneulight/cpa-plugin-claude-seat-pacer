package runtime

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// reserveYAML holds charles@ to 70% of the session, the week and the Fable
// week, naming the account by its email in mixed case.
const reserveYAML = testConfigYAML + `reserves:
  - accounts: [Charles@NeulightCo.com]
    ceilings:
      five_hour: 0.70
      seven_day: 0.70
      fable: 0.70
`

// reserveSeats lists two credentials with the host and reads them. charles is
// the live seat: its session at 70%, its week at 57% resetting in 20 hours.
// spare is far from its reset with most of its week left, so cost alone and
// the expiry rule alike prefer charles.
func (tp *testPlugin) reserveSeats(session float64) {
	tp.mu.Lock()
	tp.auths = []HostAuthFileEntry{
		{ID: "claude-charles.json", Name: "claude-charles.json", Email: "charles@neulightco.com", Provider: "claude", Status: "active"},
		{ID: "claude-spare.json", Name: "claude-spare.json", Email: "spare@neulightco.com", Provider: "claude", Status: "active"},
	}
	tp.mu.Unlock()
	tp.quota.Put(seat("claude-charles.json", testNow,
		model.Window{Kind: model.WindowSession, Utilization: session, ResetsAt: testNow.Add(2 * time.Hour), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Utilization: 0.57, ResetsAt: testNow.Add(20 * time.Hour), Duration: model.WeeklyDuration}))
	tp.quota.Put(seat("claude-spare.json", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.1, ResetsAt: testNow.Add(3 * time.Hour), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Utilization: 0.30, ResetsAt: testNow.Add(5 * 24 * time.Hour), Duration: model.WeeklyDuration}))
}

func TestReserveKeysDecode(t *testing.T) {
	want := []model.ReserveConfig{{Accounts: []string{"charles@neulightco.com"}, Ceilings: model.Ceilings{"five_hour": 0.7, "seven_day": 0.7, "fable": 0.7}}}
	for name, block := range map[string]string{
		"nested": reserveYAML,
		"flow":   testConfigYAML + "reserves: [{accounts: [charles@neulightco.com], ceilings: {five_hour: 0.7, seven_day: 0.7, Fable: 0.7}}]\n",
	} {
		cfg, warnings, err := decodeConfig([]byte(block))
		if err != nil || len(warnings) != 0 {
			t.Fatalf("%s: err %v warnings %q", name, err, warnings)
		}
		if !reflect.DeepEqual(cfg.Reserves, want) {
			t.Errorf("%s: reserves = %+v, want %+v", name, cfg.Reserves, want)
		}
	}
	cfg, _, err := decodeConfig([]byte(testConfigYAML))
	if err != nil || cfg.Reserves != nil {
		t.Errorf("absent: reserves = %+v err %v, want none", cfg.Reserves, err)
	}
}

// The live case. With the expiry rule on, charles is expiring and takes every
// new conversation; reserved, its session at the 70% ceiling holds it off and
// spare takes it. Under the ceiling, charles is expiring again: its week has
// 13% left below its ceiling.
func TestColdPickSkipsASeatAtItsReserveCeiling(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		session      float64
		want         string
		reason       string
		expiring     bool
	}{
		{"no reserve", expiryOn, 0.70, "claude-charles.json", "", true},
		{"at the ceiling", reserveYAML + "pace.expiry.enabled: true\n", 0.70, "claude-spare.json", model.ReasonReserve, false},
		{"under the ceiling", reserveYAML + "pace.expiry.enabled: true\n", 0.40, "claude-charles.json", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, tc.config)
			tp.reserveSeats(tc.session)
			resp := tp.pick(t, pickRequest(opusModel, "k", "claude-charles.json", "claude-spare.json"))
			if !resp.Handled || resp.AuthID != tc.want {
				t.Fatalf("response = %+v, want %s", resp, tc.want)
			}

			st := tp.Status(testNow, opusModel)
			for _, row := range st.Auths {
				if row.AuthID != "claude-charles.json" {
					if row.Ceilings != nil {
						t.Errorf("%s ceilings = %v, want none", row.AuthID, row.Ceilings)
					}
					continue
				}
				if row.Score.Reason != tc.reason || row.Score.Expiring != tc.expiring {
					t.Errorf("charles score = %+v, want reason %q expiring %v", row.Score, tc.reason, tc.expiring)
				}
				if reserved := tc.name != "no reserve"; reserved != (row.Ceilings != nil) {
					t.Errorf("charles ceilings = %v", row.Ceilings)
				}
			}
		})
	}
}

// A conversation bound to a seat that reaches its ceiling moves, with
// override-threshold on as by default and off, and stays when the seat is
// under it.
func TestABindingMovesOffASeatAtItsCeiling(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		session      float64
		want, kind   string
	}{
		{"at the ceiling", reserveYAML, 0.70, "claude-spare.json", model.DecisionFailover},
		{"at the ceiling, override off", reserveYAML + "affinity.override-threshold: false\n", 0.75, "claude-spare.json", model.DecisionFailover},
		{"under the ceiling", reserveYAML, 0.65, "claude-charles.json", model.DecisionAffinityHit},
		{"no reserve", testConfigYAML, 0.75, "claude-charles.json", model.DecisionAffinityHit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, tc.config)
			tp.reserveSeats(tc.session)
			tp.bindings.Bind("claude", opusModel, "k", "claude-charles.json", testNow.Add(-time.Minute))

			resp := tp.pick(t, pickRequest(opusModel, "k", "claude-charles.json", "claude-spare.json"))
			if resp.AuthID != tc.want {
				t.Fatalf("response = %+v, want %s", resp, tc.want)
			}
			d := tp.lastDecision(t)
			if d.Kind != tc.kind {
				t.Errorf("decision = %+v, want %s", d, tc.kind)
			}
			if tc.kind == model.DecisionFailover && (d.PreviousAuthID != "claude-charles.json" || !strings.Contains(d.Note, "reserve ceiling")) {
				t.Errorf("failover decision = %+v", d)
			}
			if b, _ := tp.bindings.Lookup("claude", opusModel, "k", testNow); b.AuthID != tc.want {
				t.Errorf("binding = %s, want %s", b.AuthID, tc.want)
			}
		})
	}
}

// With nowhere else to go a reserved binding stands, as a refused one does:
// moving buys nothing and alternating seats costs every request its cache.
func TestAReservedBindingStandsWithNoOtherHome(t *testing.T) {
	tp := newTestPlugin(t, reserveYAML)
	tp.reserveSeats(0.70)
	tp.bindings.Bind("claude", opusModel, "k", "claude-charles.json", testNow.Add(-time.Minute))
	resp := tp.pick(t, pickRequest(opusModel, "k", "claude-charles.json"))
	if resp.AuthID != "claude-charles.json" || !strings.Contains(tp.lastDecision(t).Note, "only seat") {
		t.Errorf("response = %+v, decision %+v", resp, tp.lastDecision(t))
	}
}

// A subagent does not follow its parent onto a seat at its ceiling.
func TestASubagentDoesNotFollowItsParentPastACeiling(t *testing.T) {
	tp := newTestPlugin(t, reserveYAML)
	tp.reserveSeats(0.70)
	tp.bindings.Bind("claude", opusModel, "parent", "claude-charles.json", testNow.Add(-time.Minute))
	req := pickRequest(opusModel, "child", "claude-charles.json", "claude-spare.json")
	req.Options.Headers[HeaderSessionParent] = []string{"parent"}
	req.Options.Headers[HeaderSubagent] = []string{"1"}
	if resp := tp.pick(t, req); resp.AuthID != "claude-spare.json" {
		t.Errorf("subagent response = %+v, want spare", resp)
	}
}

// A reserve matches a credential by file name as well as by email, and an
// account matching no listed credential raises a warning naming its position.
func TestReserveMatchingAndUnmatchedWarning(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML+`reserves:
  - accounts: [CLAUDE-CHARLES.JSON, nobody@example.com]
    ceilings: {five_hour: 0.7, monthly: 0.5}
`)
	tp.reserveSeats(0.70)
	if resp := tp.pick(t, pickRequest(opusModel, "k", "claude-charles.json", "claude-spare.json")); resp.AuthID != "claude-spare.json" {
		t.Errorf("response = %+v, want spare", resp)
	}
	st := tp.Status(testNow, opusModel)
	for _, want := range []string{
		"reserves[0].ceilings.monthly names no window",
		"reserves[0].accounts[1] matches no credential the host lists",
	} {
		found := false
		for _, w := range st.Warnings {
			found = found || strings.Contains(w, want)
		}
		if !found {
			t.Errorf("warnings %q lack %q", st.Warnings, want)
		}
	}
	for _, w := range st.Warnings {
		if strings.Contains(w, "nobody@") {
			t.Errorf("warning names the account: %q", w)
		}
	}
}

// A seat at its own ceiling is no alternative home: with every other seat
// reserved too, the binding stands rather than trading one refusal for
// another and a cache miss.
func TestAReservedSeatIsNoAlternativeHome(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML+`reserves:
  - accounts: [charles@neulightco.com, spare@neulightco.com]
    ceilings: {five_hour: 0.05}
`)
	tp.reserveSeats(0.70)
	tp.bindings.Bind("claude", opusModel, "k", "claude-charles.json", testNow.Add(-time.Minute))
	resp := tp.pick(t, pickRequest(opusModel, "k", "claude-charles.json", "claude-spare.json"))
	if d := tp.lastDecision(t); resp.AuthID != "claude-charles.json" || d.Kind != model.DecisionAffinityHit || !strings.Contains(d.Note, "no other seat") {
		t.Errorf("response = %+v, decision %+v", resp, d)
	}
}
