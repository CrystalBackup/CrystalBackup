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

package soak

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------------------------

// Event is one Warning, flattened.
//
// Warnings only. Normal events are the overwhelming majority of the volume and none of the
// signal: a fortnight of "Created pod" would fill the cap on its own and displace the raw metrics
// that §3b is trying to protect.
type Event struct {
	At        time.Time `json:"at"`
	UID       string    `json:"uid"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Name      string    `json:"name,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Count     int32     `json:"count,omitempty"`
	Component string    `json:"component,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// EventStream keeps the Kubernetes events the API server will have forgotten by the time anyone
// looks.
//
// Events expire after about an hour. That single fact is what makes a resident collector
// necessary rather than convenient: a daily job — the fallback CronJob path — sees the last hour
// of a fourteen-day soak and nothing else, which is why hack/soak/collect.sh can never grade that
// path better than INCOMPLETE.
type EventStream struct {
	cs    *kubernetes.Clientset
	store *Store

	// seen deduplicates by (UID, count). An event that recurs increments its count in place
	// rather than creating a new object, so keying on UID alone would record the first
	// occurrence of a failure that then repeated four hundred times, and keying on nothing would
	// record the same object on every relist.
	seen map[string]bool
}

// NewEventStream builds it.
func NewEventStream(cs *kubernetes.Clientset, store *Store) *EventStream {
	return &EventStream{cs: cs, store: store, seen: map[string]bool{}}
}

// Poll captures every new Warning.
//
// A LIST on a resync interval rather than a long-lived WATCH. A watch is the better shape on
// paper and the worse one here: it drops silently on an apiserver rollover, a network policy
// change or a proxy timeout, and a collector that lost its watch on day four would record nothing
// for ten days while looking perfectly healthy. Events live for an hour; a list every few minutes
// cannot miss one, and it fails LOUDLY when it fails at all.
func (s *EventStream) Poll(ctx context.Context, now time.Time) {
	var cont string
	for {
		list, err := s.cs.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: "type=Warning",
			Limit:         500,
			Continue:      cont,
		})
		if err != nil {
			s.store.RecordError(StreamEvents, "list Warning events: "+err.Error(), now)
			return
		}
		var lines [][]byte
		for i := range list.Items {
			e := &list.Items[i]
			key := fmt.Sprintf("%s/%d", e.UID, e.Count)
			if s.seen[key] {
				continue
			}
			s.seen[key] = true
			line, err := json.Marshal(flattenEvent(e))
			if err != nil {
				continue
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			if err := s.store.Append(StreamEvents, now, lines); err != nil {
				s.store.RecordError(StreamEvents, "append events: "+err.Error(), now)
			}
		}
		cont = list.Continue
		if cont == "" {
			break
		}
	}
}

func flattenEvent(e *corev1.Event) Event {
	at := e.LastTimestamp.Time
	if at.IsZero() {
		at = e.EventTime.Time
	}
	if at.IsZero() {
		at = e.CreationTimestamp.Time
	}
	return Event{
		At:        at.UTC(),
		UID:       string(e.UID),
		Namespace: e.InvolvedObject.Namespace,
		Kind:      e.InvolvedObject.Kind,
		Name:      e.InvolvedObject.Name,
		Reason:    e.Reason,
		Count:     e.Count,
		Component: e.Source.Component,
		Message:   e.Message,
	}
}

// ---------------------------------------------------------------------------------------------
// The operator's own error lines
// ---------------------------------------------------------------------------------------------

// errorLine matches an error-level log line in either of the two shapes zap can be configured to
// emit. The same two patterns hack/soak/collect.sh's fallback path uses, so the resident and the
// degraded modes select the same lines and an archive from one is comparable with an archive from
// the other.
var errorLine = regexp.MustCompile(`"level":"(error|dpanic|panic|fatal)"|[\s\t](ERROR|DPANIC|PANIC|FATAL)[\s\t]`)

// LogLine is one operator error, with the timestamp the kubelet stamped on it.
type LogLine struct {
	At   time.Time `json:"at"`
	Pod  string    `json:"pod"`
	Line string    `json:"line"`
}

// LogStream reads the operator's error-level lines continuously from the API.
//
// Continuously, rather than at the end, is the whole point: `kubectl logs` at export time is
// bounded by the kubelet's rotation, which on a busy node is hours. Reading them as they happen is
// what makes day three's error survive to day fourteen.
type LogStream struct {
	cs        *kubernetes.Clientset
	store     *Store
	namespace string
	selector  string
	// since is per-pod so an operator restart (a new pod name) starts from that pod's beginning
	// rather than from the previous pod's last line.
	since map[string]time.Time
}

// operatorSelector finds the operator's pods. `control-plane: controller-manager` is what the
// chart labels them and what its own metrics Service selects on.
const operatorSelector = "control-plane=controller-manager"

// NewLogStream builds it.
func NewLogStream(cs *kubernetes.Clientset, store *Store, namespace string) *LogStream {
	return &LogStream{
		cs: cs, store: store, namespace: namespace,
		selector: operatorSelector, since: map[string]time.Time{},
	}
}

// Poll appends whatever error lines are new.
func (l *LogStream) Poll(ctx context.Context, now time.Time) {
	pods, err := l.cs.CoreV1().Pods(l.namespace).List(ctx, listOptionsFor(l.selector))
	if err != nil {
		l.store.RecordError(StreamLogs, "list operator pods: "+err.Error(), now)
		return
	}
	for i := range pods.Items {
		l.pollPod(ctx, &pods.Items[i], now)
	}
}

func (l *LogStream) pollPod(ctx context.Context, pod *corev1.Pod, now time.Time) {
	opts := &corev1.PodLogOptions{Timestamps: true}
	if t, ok := l.since[pod.Name]; ok {
		// One second of overlap, deduplicated below by the line's own timestamp. Asking for
		// exactly the last line's timestamp drops any line that shared its second.
		s := metav1.NewTime(t.Add(-time.Second))
		opts.SinceTime = &s
	}
	stream, err := l.cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		l.store.RecordError(StreamLogs, "read operator logs: "+err.Error(), now)
		return
	}
	defer func() { _ = stream.Close() }()

	var lines [][]byte
	last := l.since[pod.Name]
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		raw := scanner.Text()
		stamp, body, ok := strings.Cut(raw, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		if !at.After(last) && !last.IsZero() {
			continue
		}
		if at.After(last) {
			last = at
		}
		if !errorLine.MatchString(body) {
			continue
		}
		line, err := json.Marshal(LogLine{At: at.UTC(), Pod: pod.Name, Line: body})
		if err != nil {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		l.store.RecordError(StreamLogs, "scan operator logs: "+err.Error(), now)
	}
	l.since[pod.Name] = last
	if len(lines) == 0 {
		return
	}
	if err := l.store.Append(StreamLogs, now, lines); err != nil {
		l.store.RecordError(StreamLogs, "append operator error lines: "+err.Error(), now)
	}
}

// ---------------------------------------------------------------------------------------------
// CR state snapshots
// ---------------------------------------------------------------------------------------------

// stateKinds is every kind in api/v1alpha1, listed as GVKs and read through unstructured.
//
// Unstructured rather than the typed scheme for the reason internal/selfcheck gives about the
// snapshot kinds: a CRD that is not installed then produces a clean, catchable no-match instead
// of a decode failure, and a soak run against a partial install records what IS there rather than
// nothing.
// kindBackup is named because it is the one kind the tests also have to say, and a kind spelled
// differently in the collector and in the test that checks it would prove nothing.
const kindBackup = "Backup"

var stateKinds = []string{
	kindBackup, "BackupSchedule", "BackupLocation", "BackupRepository", "BackupExternalSync",
	"Restore",
	"ClusterBackup", "ClusterBackupSchedule", "ClusterBackupLocation", "ClusterBackupExternalSync",
	"ClusterRestore", "ClusterErasure",
}

const crGroup = "crystalbackup.io"
const crVersion = "v1alpha1"

// StateSnapshot is one CR at one hour. The spec is NOT captured: it is configuration, every daily
// self-check carries it, and it is where the free text lives. What changes hour to hour, and what
// a fortnight is for, is the status.
type StateSnapshot struct {
	At        time.Time       `json:"at"`
	Kind      string          `json:"kind"`
	Namespace string          `json:"namespace,omitempty"`
	Name      string          `json:"name"`
	Created   time.Time       `json:"created"`
	Status    json.RawMessage `json:"status,omitempty"`
}

// StateStream snapshots the CRs on an interval.
type StateStream struct {
	reader client.Reader
	store  *Store
}

// NewStateStream builds it.
func NewStateStream(reader client.Reader, store *Store) *StateStream {
	return &StateStream{reader: reader, store: store}
}

// Snapshot writes one round of CR state.
func (s *StateStream) Snapshot(ctx context.Context, now time.Time) {
	var lines [][]byte
	for _, kind := range stateKinds {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: crGroup, Version: crVersion, Kind: kind + "List",
		})
		if err := s.reader.List(ctx, &list); err != nil {
			// A kind whose CRD is absent is not an error worth a record on every hour of a
			// fortnight; a kind the collector is not allowed to read is. Both land here, and the
			// coalescing in RecordError means either costs exactly one entry.
			s.store.RecordError(StreamState, "list "+kind+": "+err.Error(), now)
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			snap := StateSnapshot{
				At:        now.UTC(),
				Kind:      kind,
				Namespace: item.GetNamespace(),
				Name:      item.GetName(),
				Created:   item.GetCreationTimestamp().UTC(),
			}
			if status, ok := item.Object["status"]; ok {
				if body, err := json.Marshal(status); err == nil {
					snap.Status = body
				}
			}
			line, err := json.Marshal(snap)
			if err != nil {
				continue
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return
	}
	if err := s.store.Append(StreamState, now, lines); err != nil {
		s.store.RecordError(StreamState, "append CR state: "+err.Error(), now)
	}
}

// countSelfchecks counts the reports on disk. Used by the manifest, which must distinguish a
// self-check stream that produced nothing from one that was never enabled.
func countSelfchecks(dir string) ([]string, error) {
	entries, err := readDirNames(dir, dirSelfcheck)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, name := range entries {
		if strings.HasSuffix(name, ".json") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}
