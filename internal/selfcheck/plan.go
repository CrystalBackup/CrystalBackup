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

package selfcheck

import "time"

// This file declares the PLAN: what the custom resources in this cluster are going to do, in
// sentences.
//
// # Why a second view of objects the report already lists
//
// The inventory section already carries every location and every schedule, field by field. It is not
// an answer to the question this section answers. An administrator who has just applied a chart's
// example manifests can read `schedule: "0 4 * * 0"`, `keepWeekly: 4`, `pruneSchedule: "0 5 * * 0"`
// and a Ready condition, and still not know that their backups and their exclusive prune window are
// an hour apart on the same night, that four weekly snapshots is a month of history, or that the
// selector they copied matches no namespace in their cluster. Every one of those is derivable from
// fields the report already has, and none of them is visible in a table of fields.
//
// So this section derives them. It is not a prettier inventory: it holds the things a field dump
// cannot say — a cron expression turned into words AND into its next occurrence, a retention policy
// turned into a span of history, a selector RESOLVED against the live namespaces, and the validity
// problems that are only findable by cross-referencing two objects.
//
// # The next occurrence is computed, not read
//
// status.nextScheduleTime is the operator's own plan and it is in the inventory. The Next field here
// is computed from the expression by internal/schedule — the same parser the controller schedules
// with — and the two are deliberately independent. A schedule whose status says nothing (never
// reconciled, or reconciled by a controller that is now wedged) still gets an answer here, and a
// disagreement between the two is itself a finding. The product owner has worked out "when is the
// next heavy maintenance?" by hand from a cron expression more than once; that is the calculation
// this field retires.

// Plan is the CR narration: what each location and schedule is configured to do.
type Plan struct {
	Locations []PlannedLocation `json:"locations,omitempty"`
	Schedules []PlannedSchedule `json:"schedules,omitempty"`
	// Note is set when there is nothing to narrate, and it says which of the two nothings it is: a
	// cluster with no location has not been configured, a cluster with a location and no schedule has
	// been configured and will never run. Reporting an empty section for both would leave the second —
	// far more dangerous — case indistinguishable from the first.
	Note string `json:"note,omitempty"`
}

// PlannedLocation is one BackupLocation or ClusterBackupLocation, described.
type PlannedLocation struct {
	Scope     string `json:"scope"` // cluster | namespace
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Default   bool   `json:"default,omitempty"`
	// Ready is the Ready condition as "Status (Reason)", or empty when the object carries none —
	// which for a location means the operator has not reconciled it yet and is itself worth showing.
	Ready string `json:"ready,omitempty"`
	// Sentences are the plain-language facts, in reading order: where the data goes, how long it is
	// kept, whether the repository is inventoried.
	Sentences []string `json:"sentences,omitempty"`
	// Maintenance is the check/prune plan. Cluster locations only: BackupLocationSpec has no
	// maintenance field at all, and an empty list on a namespace location means exactly that rather
	// than "unconfigured".
	Maintenance []PlannedMaintenance `json:"maintenance,omitempty"`
	// Problems are the findings an administrator has to act on.
	Problems []string `json:"problems,omitempty"`
}

// PlannedMaintenance is one repository-maintenance window.
type PlannedMaintenance struct {
	// Operation is "check" or "prune".
	Operation string `json:"operation"`
	Cron      string `json:"cron"`
	// InWords is the expression in English; Next the first occurrence after the report's instant.
	InWords string     `json:"inWords,omitempty"`
	Next    *time.Time `json:"next,omitempty"`
	// Exclusive marks the prune window. It is the one operation during which NO namespace in the
	// cluster can start a backup (adr/0009: one shared repository, one prune window), which makes it
	// the "heavy maintenance" an administrator plans their evening around — and the reason this
	// section computes a next occurrence at all.
	Exclusive bool `json:"exclusive,omitempty"`
	// Detail carries the knobs that decide how long the window lasts and how much it actually
	// verifies: the repack cap for a prune, the read-data subset for a check.
	Detail string `json:"detail,omitempty"`
}

// PlannedSchedule is one BackupSchedule or ClusterBackupSchedule, described.
type PlannedSchedule struct {
	Origin    string `json:"origin"` // cluster | namespace
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Cron      string `json:"cron,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	// InWords is the expression in English, and Next the first occurrence after the report's instant
	// — computed from the expression, not read from status. See this file's header.
	InWords string     `json:"inWords,omitempty"`
	Next    *time.Time `json:"next,omitempty"`
	// Suspended mirrors spec.paused. It is a field of its own and not merely a Problem because it is
	// a legitimate operational state as often as it is a mistake, and the report has no business
	// deciding which.
	Suspended bool   `json:"suspended,omitempty"`
	Ready     string `json:"ready,omitempty"`
	// Sentences are the plain-language facts: what it targets, where it writes, what it also captures.
	Sentences []string `json:"sentences,omitempty"`
	// Problems are the findings: a selector that matches nothing, a location that does not exist, a
	// cron that does not parse, a run that has never succeeded.
	Problems []string `json:"problems,omitempty"`
}

// planNoLocations and planNoSchedules are the two nothings Plan.Note distinguishes.
const (
	planNoLocations = "No BackupLocation or ClusterBackupLocation exists, so this installation has " +
		"no destination and nothing can be backed up yet. Create a ClusterBackupLocation to give the " +
		"operator somewhere to write."
	planNoSchedules = "A location exists but NO schedule does, so nothing will ever run on its own. " +
		"Backups will only happen when someone creates a Backup or ClusterBackup by hand."
)
