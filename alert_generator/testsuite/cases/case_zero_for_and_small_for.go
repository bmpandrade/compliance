package cases

import (
	"fmt"
	"github.com/prometheus/prometheus/notifier"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/web/api/v1"
	"gopkg.in/yaml.v3"
)

// ZeroFor_SmallFor tests the following cases:
// * Alert that goes directly to firing state (skipping the pending state) because of zero for duration.
// * When the for duration is non-zero and less than the evaluation interval, firing alert must be sent
//   after the second evaluation of the rule and not before.
// * Alert that becomes active after having fired already and gone into inactive state where for duration
//   is zero and the inactive alert was not being sent anymore.
// * Alert goes into inactive when there is no more data when in firing.
func ZeroFor_SmallFor() TestCase {
	groupName := "ZeroFor_SmallFor"
	zfAlertName := groupName + "_ZeroFor"
	sfAlertName := groupName + "_SmallFor"
	zfLabels := metricLabels(groupName, zfAlertName)
	sfLabels := metricLabels(groupName, sfAlertName)
	tc := &zeroAndSmallFor{
		groupName:      groupName,
		zfAlertName:    zfAlertName,
		zfQuery:        fmt.Sprintf("%s > 10", zfLabels.String()),
		zfMetricLabels: zfLabels,
		sfAlertName:    sfAlertName,
		sfQuery:        fmt.Sprintf("%s > 13", sfLabels.String()),
		sfMetricLabels: sfLabels,
		// TODO: make this 15 and 30 for final use.
		rwInterval:    5 * time.Second,
		groupInterval: 10 * time.Second,
	}
	tc.forDuration = model.Duration(tc.groupInterval / 2)
	return tc
}

type zeroAndSmallFor struct {
	groupName                      string
	zfAlertName, sfAlertName       string
	zfQuery, sfQuery               string
	zfMetricLabels, sfMetricLabels labels.Labels
	rwInterval, groupInterval      time.Duration
	forDuration                    model.Duration // For the "small for".
	totalSamples                   int

	zeroTime int64
}

func (tc *zeroAndSmallFor) Describe() (title string, description string) {
	return tc.groupName,
		"(1) Alert that goes directly to firing state (skipping the pending state) because of zero for duration. " +
			"(2) When the for duration is non-zero and less than the evaluation interval, firing alert must be sent after the second evaluation of the rule and not before. " +
			"(3) Alert that becomes active after having fired already and gone into inactive state where 'for' duration is zero and the inactive alert was not being sent anymore." +
			"(4) Alert goes into inactive when there is no more data when in firing."
}

func (tc *zeroAndSmallFor) RuleGroup() (rulefmt.RuleGroup, error) {
	var zfAlert, sfAlert yaml.Node
	if err := zfAlert.Encode(tc.zfAlertName); err != nil {
		return rulefmt.RuleGroup{}, err
	}
	if err := sfAlert.Encode(tc.sfAlertName); err != nil {
		return rulefmt.RuleGroup{}, err
	}
	var zfExpr, sfExpr yaml.Node
	if err := zfExpr.Encode(tc.zfQuery); err != nil {
		return rulefmt.RuleGroup{}, err
	}
	if err := sfExpr.Encode(tc.sfQuery); err != nil {
		return rulefmt.RuleGroup{}, err
	}
	return rulefmt.RuleGroup{
		Name:     tc.groupName,
		Interval: model.Duration(tc.groupInterval),
		Rules: []rulefmt.RuleNode{
			{ // Zero for.
				Alert:  zfAlert,
				Expr:   zfExpr,
				Labels: map[string]string{"foo": "bar", "rulegroup": tc.groupName},
				Annotations: map[string]string{
					"description":   "This should immediately fire",
					"template_test": "{{humanize 1048576}} {{humanize1024 1048576}} {{humanizeDuration 135.3563}} {{humanizePercentage 0.959}} {{humanizeTimestamp 1643114203}}",
				},
			},
			{ // Small for.
				Alert:  sfAlert,
				Expr:   sfExpr,
				For:    tc.forDuration,
				Labels: map[string]string{"ba_dum": "tss", "rulegroup": tc.groupName},
				Annotations: map[string]string{
					"description":   "This should fire after an interval",
					"template_test": `{{title "this part"}} {{toUpper "is testing"}} {{toLower "THE STRINGS"}}. {{if match "[0-9]+" "1234"}}{{reReplaceAll "r.*d" "replaced" "rpld text"}}{{end}}. {{if match "[0-9]+$" "1234a"}}WRONG{{end}}.`,
				},
			},
		},
	}, nil
}

func (tc *zeroAndSmallFor) SamplesToRemoteWrite() []prompb.TimeSeries {
	samples := sampleSlice(tc.rwInterval,
		// All comment times is assuming 15s interval.
		"3", "5", "0x2", "9", // 1m (3 is @0 time).
		"0x3", "15", // 1m block. Gets into firing or pending at value 15@2m.
		"0x12", // 3m of active state.
		// Resolved. 18m more of 9s. Should not get inactive alerts after 15m of this.
		"9", "0x71",
		"11", "0x12", // Zero 'for' alert goes into firing again. ~3m of this.
		"9", // Resolved again.
	)
	tc.totalSamples = len(samples) + 20 // We want to wait for 5m more to see inactive alerts.
	return []prompb.TimeSeries{
		{
			Labels:  toProtoLabels(tc.zfMetricLabels),
			Samples: samples,
		},
		{
			Labels:  toProtoLabels(tc.sfMetricLabels),
			Samples: samples,
		},
	}
}

func (tc *zeroAndSmallFor) Init(zt int64) {
	tc.zeroTime = zt
}

func (tc *zeroAndSmallFor) TestUntil() int64 {
	return timestamp.FromTime(timestamp.Time(tc.zeroTime).Add(time.Duration(tc.totalSamples) * tc.rwInterval))
}

func (tc *zeroAndSmallFor) CheckAlerts(ts int64, alerts []v1.Alert) error {
	expAlerts := tc.expAlerts(ts, alerts)
	return checkExpectedAlerts(expAlerts, alerts, tc.groupInterval)
}

func (tc *zeroAndSmallFor) CheckRuleGroup(ts int64, rg *v1.RuleGroup) error {
	if ts-tc.zeroTime < int64(tc.groupInterval/time.Millisecond) {
		// We wait till 1 evaluation is done.
		return nil
	}
	if rg == nil {
		return errors.New("no rule group found")
	}
	expRgs := tc.expRuleGroups(ts)
	return checkExpectedRuleGroup(timestamp.Time(ts), expRgs, *rg)
}

func (tc *zeroAndSmallFor) CheckMetrics(ts int64, samples []promql.Sample) error {
	expSamples := tc.expMetrics(ts)
	return checkExpectedSamples(expSamples, samples)
}

func (tc *zeroAndSmallFor) expAlerts(ts int64, alerts []v1.Alert) (expAlerts [][]v1.Alert) {
	relTs := ts - tc.zeroTime
	canBeInactive, zfFiring, zfFiringAgain, sfPending, sfFiring := tc.allPossibleStates(relTs)
	activeAt := timestamp.Time(tc.zeroTime + int64(8*tc.rwInterval/time.Millisecond))
	activeAt2 := timestamp.Time(tc.zeroTime + int64(93*tc.rwInterval/time.Millisecond))

	desc := "-----"
	if canBeInactive {
		expAlerts = append(expAlerts, []v1.Alert{})
		desc += "/inactive"
	}
	if zfFiring && sfPending {
		expAlerts = append(expAlerts, []v1.Alert{
			{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				State:       "firing",
				Value:       "15",
				ActiveAt:    &activeAt,
			},
			{
				Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
				State:       "pending",
				Value:       "15",
				ActiveAt:    &activeAt,
			},
		})
		desc += "/firing/pending"
	}
	if zfFiring && sfFiring {
		expAlerts = append(expAlerts, []v1.Alert{
			{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				State:       "firing",
				Value:       "15",
				ActiveAt:    &activeAt,
			},
			{
				Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
				State:       "firing",
				Value:       "15",
				ActiveAt:    &activeAt,
			},
		})
		desc += "/firing/firing"
	}
	if zfFiringAgain {
		expAlerts = append(expAlerts, []v1.Alert{
			{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				State:       "firing",
				Value:       "11",
				ActiveAt:    &activeAt2,
			},
		})
		desc += "/firing_again"
	}

	// TODO: temporary for development.
	fmt.Println(desc, alerts)

	return expAlerts
}

func (tc *zeroAndSmallFor) expRuleGroups(ts int64) (expRgs []v1.RuleGroup) {
	relTs := ts - tc.zeroTime
	canBeInactive, zfFiring, zfFiringAgain, sfPending, sfFiring := tc.allPossibleStates(relTs)
	activeAt := timestamp.Time(tc.zeroTime + int64(8*tc.rwInterval/time.Millisecond))
	activeAt2 := timestamp.Time(tc.zeroTime + int64(93*tc.rwInterval/time.Millisecond))

	getRg := func(s1, s2 string, a1, a2 []*v1.Alert) v1.RuleGroup {
		return v1.RuleGroup{
			Name:     tc.groupName,
			Interval: float64(tc.groupInterval / time.Second),
			Rules: []v1.Rule{
				v1.AlertingRule{
					State:  s1,
					Name:   tc.zfAlertName,
					Query:  tc.zfQuery,
					Labels: labels.FromStrings("foo", "bar", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings(
						"description", "This should immediately fire",
						"template_test", "{{humanize 1048576}} {{humanize1024 1048576}} {{humanizeDuration 135.3563}} {{humanizePercentage 0.959}} {{humanizeTimestamp 1643114203}}",
					),
					Alerts: a1,
					Health: "ok",
					Type:   "alerting",
				},
				v1.AlertingRule{
					State:    s2,
					Name:     tc.sfAlertName,
					Query:    tc.sfQuery,
					Duration: float64(time.Duration(tc.forDuration) / time.Second),
					Labels:   labels.FromStrings("ba_dum", "tss", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings(
						"description", "This should fire after an interval",
						"template_test", `{{title "this part"}} {{toUpper "is testing"}} {{toLower "THE STRINGS"}}. {{if match "[0-9]+" "1234"}}{{reReplaceAll "r.*d" "replaced" "rpld text"}}{{end}}. {{if match "[0-9]+$" "1234a"}}WRONG{{end}}.`,
					),
					Alerts: a2,
					Health: "ok",
					Type:   "alerting",
				},
			},
		}
	}

	if canBeInactive {
		expRgs = append(expRgs, getRg("inactive", "inactive", nil, nil))
	}
	if zfFiring && sfPending {
		expRgs = append(expRgs, getRg("firing", "pending",
			[]*v1.Alert{
				{
					Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
					State:       "firing",
					Value:       "15",
					ActiveAt:    &activeAt,
				},
			},
			[]*v1.Alert{
				{
					Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
					State:       "pending",
					Value:       "15",
					ActiveAt:    &activeAt,
				},
			},
		))
	}
	if zfFiring && sfFiring {
		expRgs = append(expRgs, getRg("firing", "firing",
			[]*v1.Alert{
				{
					Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
					State:       "firing",
					Value:       "15",
					ActiveAt:    &activeAt,
				},
			},
			[]*v1.Alert{
				{
					Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
					State:       "firing",
					Value:       "15",
					ActiveAt:    &activeAt,
				},
			},
		))
	}
	if zfFiringAgain {
		expRgs = append(expRgs, getRg("firing", "inactive",
			[]*v1.Alert{
				{
					Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
					Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
					State:       "firing",
					Value:       "11",
					ActiveAt:    &activeAt2,
				},
			}, nil,
		))
	}

	return expRgs
}

func (tc *zeroAndSmallFor) expMetrics(ts int64) (expSamples [][]promql.Sample) {
	relTs := ts - tc.zeroTime
	canBeInactive, zfFiring, zfFiringAgain, sfPending, sfFiring := tc.allPossibleStates(relTs)

	if canBeInactive {
		expSamples = append(expSamples, nil)
	}
	if zfFiring && sfPending {
		expSamples = append(expSamples, []promql.Sample{
			{
				Point:  promql.Point{T: ts / 1000, V: 1},
				Metric: labels.FromStrings("__name__", "ALERTS", "alertstate", "firing", "alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
			},
			{
				Point:  promql.Point{T: ts / 1000, V: 1},
				Metric: labels.FromStrings("__name__", "ALERTS", "alertstate", "pending", "alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
			},
		})
	}
	if zfFiring && sfFiring {
		expSamples = append(expSamples, []promql.Sample{
			{
				Point:  promql.Point{T: ts / 1000, V: 1},
				Metric: labels.FromStrings("__name__", "ALERTS", "alertstate", "firing", "alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
			},
			{
				Point:  promql.Point{T: ts / 1000, V: 1},
				Metric: labels.FromStrings("__name__", "ALERTS", "alertstate", "firing", "alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
			},
		})
	}
	if zfFiringAgain {
		expSamples = append(expSamples, []promql.Sample{
			{
				Point:  promql.Point{T: ts / 1000, V: 1},
				Metric: labels.FromStrings("__name__", "ALERTS", "alertstate", "firing", "alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
			},
		})
	}

	return expSamples
}

// ts is relative time w.r.t. zeroTime.
func (tc *zeroAndSmallFor) allPossibleStates(ts int64) (canBeInactive, zfFiring, zfFiringAgain, sfPending, sfFiring bool) {
	between := betweenFunc(ts)

	rwItvlSecFloat, grpItvlSecFloat := float64(tc.rwInterval/time.Second), float64(tc.groupInterval/time.Second)
	_8th := 8 * rwItvlSecFloat     // Goes into pending.
	_21st := 21 * rwItvlSecFloat   // Becomes inactive.
	_93rd := 93 * rwItvlSecFloat   // Firing again.
	_106th := 106 * rwItvlSecFloat // Resolved again.

	canBeInactive = between(0, _8th+grpItvlSecFloat) ||
		between(_21st-1, _93rd+grpItvlSecFloat) ||
		between(_106th, 240*rwItvlSecFloat)

	zfFiring = between(_8th-1, _21st+grpItvlSecFloat)
	zfFiringAgain = between(_93rd-1, _106th+grpItvlSecFloat)

	sfPending = between(_8th-1, _8th+(2*grpItvlSecFloat))
	sfFiring = between(_8th+grpItvlSecFloat, _21st+grpItvlSecFloat)
	return
}

func (tc *zeroAndSmallFor) ExpectedAlerts() []ExpectedAlert {
	_8th := 8 * int64(tc.rwInterval/time.Millisecond)               // Zero for firing.
	_8th_plus_gi := _8th + int64(tc.groupInterval/time.Millisecond) // Small for firing.
	_21st := 21 * int64(tc.rwInterval/time.Millisecond)             // Resolved.
	_21stPlus15m := _21st + int64(15*time.Minute/time.Millisecond)
	_93rd := 93 * int64(tc.rwInterval/time.Millisecond)   // Firing again.
	_106th := 106 * int64(tc.rwInterval/time.Millisecond) // Resolved again.
	_106thPlus15m := _106th + int64(15*time.Minute/time.Millisecond)

	var exp []ExpectedAlert
	endsAtDelta := 4 * ResendDelay
	if endsAtDelta < 4*tc.groupInterval {
		endsAtDelta = 4 * tc.groupInterval
	}

	orderingID := 0
	addAlert := func(ea ExpectedAlert) {
		orderingID++
		ea.OrderingID = orderingID
		exp = append(exp, ea)
	}

	resendDelayMs := int64(ResendDelay / time.Millisecond)
	// Zero for.
	for ts := _8th; ts < _21st; ts += resendDelayMs {
		addAlert(ExpectedAlert{
			TimeTolerance: tc.groupInterval,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      false,
			Resend:        ts != _8th,
			NextState:     timestamp.Time(tc.zeroTime + _21st),
			ResolvedTime:  timestamp.Time(tc.zeroTime + _21st),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				StartsAt:    timestamp.Time(tc.zeroTime + _8th),
			},
		})
	}
	for ts := _21st; ts < _21stPlus15m; ts += resendDelayMs {
		tolerance := tc.groupInterval
		if ts == _21st {
			// Since the alert state is reset, the alert sent time for resolved alert can be upto
			// 1 groupInterval late compared to actual time when it gets resolved. So we need to
			// account for this delay plus the usual tolerance.
			// We don't change tolerance for other resolved alerts because their Ts will be adjusted
			// based on this first resolved alert.
			tolerance = 2 * tc.groupInterval
		}
		addAlert(ExpectedAlert{
			TimeTolerance: tolerance,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      true,
			Resend:        ts != _21st,
			NextState:     timestamp.Time(tc.zeroTime + _93rd),
			ResolvedTime:  timestamp.Time(tc.zeroTime + _21st),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				StartsAt:    timestamp.Time(tc.zeroTime + _8th),
			},
		})
	}
	for ts := _93rd; ts < _106th; ts += resendDelayMs {
		addAlert(ExpectedAlert{
			TimeTolerance: tc.groupInterval,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      false,
			Resend:        ts != _93rd,
			NextState:     timestamp.Time(tc.zeroTime + _106th),
			ResolvedTime:  timestamp.Time(tc.zeroTime + _106th),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				StartsAt:    timestamp.Time(tc.zeroTime + _93rd),
			},
		})
	}
	for ts := _106th; ts < _106thPlus15m; ts += resendDelayMs {
		tolerance := tc.groupInterval
		if ts == _106th {
			// Since the alert state is reset, the alert sent time for resolved alert can be upto
			// 1 groupInterval late compared to actual time when it gets resolved. So we need to
			// account for this delay plus the usual tolerance.
			// We don't change tolerance for other resolved alerts because their Ts will be adjusted
			// based on this first resolved alert.
			tolerance = 2 * tc.groupInterval
		}
		addAlert(ExpectedAlert{
			TimeTolerance: tolerance,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      true,
			Resend:        ts != _106th,
			ResolvedTime:  timestamp.Time(tc.zeroTime + _106th),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.zfAlertName, "foo", "bar", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should immediately fire", "template_test", "1.049M 1Mi 2m 15s 95.9% 2022-01-25 12:36:43 +0000 UTC"),
				StartsAt:    timestamp.Time(tc.zeroTime + _93rd),
			},
		})
	}

	// Small for.
	for ts := _8th_plus_gi; ts < _21st; ts += resendDelayMs {
		addAlert(ExpectedAlert{
			TimeTolerance: tc.groupInterval,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      false,
			Resend:        ts != _8th_plus_gi,
			NextState:     timestamp.Time(tc.zeroTime + _21st),
			ResolvedTime:  timestamp.Time(tc.zeroTime + _21st),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
				StartsAt:    timestamp.Time(tc.zeroTime + _8th_plus_gi),
			},
		})
	}
	for ts := _21st; ts < _21stPlus15m; ts += resendDelayMs {
		tolerance := tc.groupInterval
		if ts == _21st {
			// Since the alert state is reset, the alert sent time for resolved alert can be upto
			// 1 groupInterval late compared to actual time when it gets resolved. So we need to
			// account for this delay plus the usual tolerance.
			// We don't change tolerance for other resolved alerts because their Ts will be adjusted
			// based on this first resolved alert.
			tolerance = 2 * tc.groupInterval
		}
		addAlert(ExpectedAlert{
			TimeTolerance: tolerance,
			Ts:            timestamp.Time(tc.zeroTime + ts),
			Resolved:      true,
			Resend:        ts != _21st,
			ResolvedTime:  timestamp.Time(tc.zeroTime + _21st),
			EndsAtDelta:   endsAtDelta,
			Alert: &notifier.Alert{
				Labels:      labels.FromStrings("alertname", tc.sfAlertName, "ba_dum", "tss", "rulegroup", tc.groupName),
				Annotations: labels.FromStrings("description", "This should fire after an interval", "template_test", "This Part IS TESTING the strings. replaced text. ."),
				StartsAt:    timestamp.Time(tc.zeroTime + _8th_plus_gi),
			},
		})
	}

	return exp
}
