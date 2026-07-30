/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// The right-to-erasure family's two gauges (spec/05-observability.md §2.6). Their counter
// siblings — snapshots forgotten, bytes reclaimed — are recorded at the event in events.go,
// because a completed erasure's whole purpose is that the data it counted is gone.
//
// erasure_blocked is the one that matters operationally, and it is a legal deadline, not a
// technical one: on an Immutable location (adr/0005) the object lock physically refuses the
// delete until it expires, so a GDPR erasure request parks in Blocked and an operator has to know
// about it long before anyone asks why the data is still there.
var (
	erasureBlockedDesc = prometheus.NewDesc(
		NameErasureBlocked,
		"ClusterErasure objects currently Blocked on this location (Immutable object-lock not yet expired). Drives ErasureBlocked.",
		erasureLabels, nil)
	erasureLastCompletionDesc = prometheus.NewDesc(
		NameErasureLastCompletion,
		"Unix time of the last Completed erasure on this location.",
		erasureLabels, nil)
)

// erasurePhaseBlocked mirrors the one ClusterErasure phase no other family shares. Its Completed
// sibling is the package-wide phaseCompleted (events.go).
const erasurePhaseBlocked = "Blocked"

type erasureSeries struct {
	blocked            float64
	lastCompletionUnix float64
}

// collectErasures emits, per location, how many erasures are stuck and when the last one finished.
//
// A location with no ClusterErasure at all emits nothing. A location that HAS one emits blocked=0
// even when nothing is stuck, which is the deliberate opposite of the never-measured rule
// elsewhere in this package: zero here is a measurement ("this location has erasure activity and
// none of it is blocked"), whereas the absent series on an untouched location says the honest
// "nobody has ever asked this location to erase anything".
func collectErasures(ch chan<- prometheus.Metric, erasures []cbv1.ClusterErasure, clusterByLocation map[string]string) {
	series := map[string]*erasureSeries{}
	for i := range erasures {
		er := &erasures[i]
		location := er.Spec.LocationRef.Name
		s := series[location]
		if s == nil {
			s = &erasureSeries{}
			series[location] = s
		}
		switch er.Status.Phase {
		case erasurePhaseBlocked:
			s.blocked++
		case phaseCompleted:
			// ClusterErasureStatus carries no completion timestamp, so the terminal transition of
			// the Ready condition is the timestamp — the same source the restore family uses for
			// the same missing field. It is the moment the phase was written, which is what the
			// question "when was the last erasure done" is asking.
			if t := terminalTime(er.Status.Conditions); t > s.lastCompletionUnix {
				s.lastCompletionUnix = t
			}
		}
	}
	for location, s := range series {
		labels := []string{location, clusterByLocation[location]}
		ch <- prometheus.MustNewConstMetric(erasureBlockedDesc, prometheus.GaugeValue, s.blocked, labels...)
		if s.lastCompletionUnix > 0 {
			ch <- prometheus.MustNewConstMetric(erasureLastCompletionDesc, prometheus.GaugeValue, s.lastCompletionUnix, labels...)
		}
	}
}
