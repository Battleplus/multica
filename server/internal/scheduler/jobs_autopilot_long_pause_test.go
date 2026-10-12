package scheduler

import (
	"context"
	"testing"
	"time"
)

// planAutopilotTrigger runs the autopilot PlansForScope hook for a single
// trigger whose last stored plan_time is lastPlan.
func planAutopilotTrigger(t *testing.T, cron string, lastPlan, now time.Time) []time.Time {
	t.Helper()
	cache := newAutopilotScheduleCache()
	cache.replace(map[string]autopilotTriggerConfig{
		"trigger": {TriggerID: "trigger", CronExpression: cron, Timezone: "UTC"},
	})
	plans, err := autopilotPlansForScope(cache)(
		context.Background(),
		Scope{Kind: ScopeKindAutopilotTrigger, ID: "trigger"},
		now,
		LatestPlanInfo{Found: true, PlanTime: lastPlan, Status: "SUCCESS"},
	)
	if err != nil {
		t.Fatalf("plans hook: %v", err)
	}
	return plans
}

// TestAutopilotPlansForScopeDenseCronAfterLongPause is the regression guard
// for a trigger that resumes after a pause longer than 1024 of its own
// occurrences. NextOccurrencesUTC keeps the FIRST 1024 activations of
// (lastPlan, now], so for "* * * * *" the last kept one sits hours in the
// past, fails the lateness guard, and the hook returns nothing. Nothing is
// written to sys_cron_executions, lastPlan never moves, and the trigger
// stays silent on every later tick while next_run_at keeps looking valid.
func TestAutopilotPlansForScopeDenseCronAfterLongPause(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 5, 10, 0, time.UTC)

	cases := []struct {
		name     string
		cron     string
		lastPlan time.Time
		want     time.Time
	}{
		{
			name:     "every minute, 18h pause (1080 occurrences)",
			cron:     "* * * * *",
			lastPlan: time.Date(2026, 9, 3, 22, 5, 0, 0, time.UTC),
			want:     time.Date(2026, 9, 4, 16, 5, 0, 0, time.UTC),
		},
		{
			name:     "every minute, 3-day pause",
			cron:     "* * * * *",
			lastPlan: time.Date(2026, 9, 1, 16, 5, 0, 0, time.UTC),
			want:     time.Date(2026, 9, 4, 16, 5, 0, 0, time.UTC),
		},
		{
			name:     "every minute, short pause",
			cron:     "* * * * *",
			lastPlan: time.Date(2026, 9, 4, 15, 55, 0, 0, time.UTC),
			want:     time.Date(2026, 9, 4, 16, 5, 0, 0, time.UTC),
		},
		{
			name:     "every two minutes, 18h pause (540 occurrences)",
			cron:     "*/2 * * * *",
			lastPlan: time.Date(2026, 9, 3, 22, 5, 0, 0, time.UTC),
			want:     time.Date(2026, 9, 4, 16, 4, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans := planAutopilotTrigger(t, tc.cron, tc.lastPlan, now)
			if len(plans) != 1 || !plans[0].Equal(tc.want) {
				t.Fatalf("plans = %v, want [%s]", plans, tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestAutopilotStuckTriggerSelfHealsWithoutManualRecovery replays the exact
// #8939 stuck state and proves recovery is automatic: the stale anchor is
// floored into the lateness window on the very next tick, and the healed
// anchor keeps producing plans afterwards. The report's recovery advice
// (change the cron to a sparser one, or delete and recreate the trigger) is
// therefore no longer needed once this fix is deployed.
func TestAutopilotStuckTriggerSelfHealsWithoutManualRecovery(t *testing.T) {
	// Measured on multica.ai 2026-09-04 16:04 UTC: cron "* * * * *", last
	// stored plan 2026-09-03 22:05 (≈1080 occurrences back), next_run_at
	// stuck, zero autopilot_run rows for 10 minutes.
	firstTick := time.Date(2026, 9, 4, 16, 5, 10, 0, time.UTC)
	silentSince := time.Date(2026, 9, 3, 22, 5, 0, 0, time.UTC)

	plans := planAutopilotTrigger(t, "* * * * *", silentSince, firstTick)
	wantFirst := time.Date(2026, 9, 4, 16, 5, 0, 0, time.UTC)
	if len(plans) != 1 || !plans[0].Equal(wantFirst) {
		t.Fatalf("first tick after the fix: plans = %v, want [%s]", plans, wantFirst.Format(time.RFC3339))
	}

	// The dispatch stores plan_time, so the next tick resumes from the
	// healed anchor and keeps firing on the normal minute cadence.
	nextTick := firstTick.Add(time.Minute)
	wantNext := wantFirst.Add(time.Minute)
	plans = planAutopilotTrigger(t, "* * * * *", plans[0], nextTick)
	if len(plans) != 1 || !plans[0].Equal(wantNext) {
		t.Fatalf("second tick: plans = %v, want [%s]", plans, wantNext.Format(time.RFC3339))
	}
}

// TestAutopilotPlansForScopeFutureAnchorCannotSilenceTrigger guards the other
// permanent-silence shape: an anchor past `now` (DB clock rollback, or a row
// written by an instance whose clock ran ahead) makes (after, now] empty on
// every tick, so the trigger stays silent until the wall clock passes the
// stored plan_time — indefinitely for a far-future row. Such an anchor now
// recovers through the same lateness floor as a stale one, and the guard
// still bounds what that recovery may dispatch.
func TestAutopilotPlansForScopeFutureAnchorCannotSilenceTrigger(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 5, 10, 0, time.UTC)
	current := time.Date(2026, 9, 4, 16, 5, 0, 0, time.UTC)

	if plans := planAutopilotTrigger(t, "* * * * *", now.Add(6*time.Hour), now); len(plans) != 1 || !plans[0].Equal(current) {
		t.Fatalf("future anchor: plans = %v, want [%s]", plans, current.Format(time.RFC3339))
	}
	// An anchor exactly at `now` is a healthy already-claimed slot: stay
	// quiet this tick (no duplicate claim of the same occurrence) and fire
	// the next one.
	if plans := planAutopilotTrigger(t, "* * * * *", now, now); len(plans) != 0 {
		t.Fatalf("anchor at now: plans = %v, want none this tick", plans)
	}
	next := current.Add(time.Minute)
	if plans := planAutopilotTrigger(t, "* * * * *", current, now.Add(time.Minute)); len(plans) != 1 || !plans[0].Equal(next) {
		t.Fatalf("steady state past an anchor at now: plans = %v, want [%s]", plans, next.Format(time.RFC3339))
	}
}

// TestAutopilotPlansForScopeKeepsLatenessGuard pins the stale catch-up rule
// the fix must not loosen: the latest due occurrence fires only while it is
// at most maxAutopilotScheduleLateness old.
func TestAutopilotPlansForScopeKeepsLatenessGuard(t *testing.T) {
	lastPlan := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	due := time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC)

	if plans := planAutopilotTrigger(t, "0 * * * *", lastPlan, due.Add(maxAutopilotScheduleLateness)); len(plans) != 1 || !plans[0].Equal(due) {
		t.Fatalf("exactly at the lateness bound: plans = %v, want [%s]", plans, due.Format(time.RFC3339))
	}
	if plans := planAutopilotTrigger(t, "0 * * * *", lastPlan, due.Add(maxAutopilotScheduleLateness+time.Second)); len(plans) != 0 {
		t.Fatalf("past the lateness bound: plans = %v, want none", plans)
	}
}
