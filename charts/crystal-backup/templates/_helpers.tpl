{{/*
Chart name (optionally overridden). Kept short for label values.
*/}}
{{- define "crystal-backup.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified base name for resources.

Crystal Backup is a singleton cluster operator, so the base name is STABLE and NOT
release-prefixed: cluster-scoped RBAC objects (crystal-backup-operator, -tenant, -admin)
must have predictable names for platform binding/aggregation and for the golden-file test
that pins the `crystal-backup-tenant` ClusterRole (spec/08 DoD #5). Override with
`fullnameOverride` only if you must run more than one instance in a cluster.
*/}}
{{- define "crystal-backup.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Operator namespace (fixed target: crystal-backup-system).
*/}}
{{- define "crystal-backup.namespace" -}}
{{- default "crystal-backup-system" .Values.namespace.name -}}
{{- end -}}

{{/*
Chart label value "name-version".
*/}}
{{- define "crystal-backup.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels stamped on every object.
*/}}
{{- define "crystal-backup.labels" -}}
helm.sh/chart: {{ include "crystal-backup.chart" . }}
{{ include "crystal-backup.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: crystal-backup
app.kubernetes.io/component: operator
{{- end -}}

{{/*
Selector labels — the immutable subset used in Deployment selectors and Service
selectors. `control-plane: controller-manager` is added alongside these where a
kubebuilder/Prometheus-style selector is expected.
*/}}
{{- define "crystal-backup.selectorLabels" -}}
app.kubernetes.io/name: {{ include "crystal-backup.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Operator ServiceAccount name (defaults to "<fullname>-operator").
*/}}
{{- define "crystal-backup.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-operator" (include "crystal-backup.fullname" .)) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Soak collector labels (soak.yaml). Not "crystal-backup.labels": that one hardcodes
app.kubernetes.io/component: operator, and the collector is a neighbour of the operator
rather than part of it — it holds its own identity and its own RBAC precisely so that
revoking it is deleting one binding.
*/}}
{{- define "crystal-backup.soak.labels" -}}
helm.sh/chart: {{ include "crystal-backup.chart" . }}
app.kubernetes.io/name: {{ include "crystal-backup.name" . }}
{{ include "crystal-backup.soak.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: crystal-backup
app.kubernetes.io/component: soak
{{- end -}}

{{/*
Soak collector selector labels — the pod's identity, and what the two soak NetworkPolicies
select on.

`app.kubernetes.io/name` is deliberately NOT here. name+instance is the podSelector of the
chart's `-operator` NetworkPolicy and (with control-plane) the selector of the metrics
Service; a collector wearing them would be confined by a policy written for a different pod
and offered as a scrape target that serves no metrics. The instance label is what keeps two
installs in one cluster from selecting each other's collector.
*/}}
{{- define "crystal-backup.soak.selectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
crystalbackup.io/soak: collector
{{- end -}}

{{/*
Manifest mover ServiceAccount name (defaults to "<fullname>-manifest-mover").

Named after the chart release like every other cluster-visible object, so two installs in one
cluster cannot collide on it. spec/03 §5 calls this identity "crystal-manifest-mover"; that is
the ROLE, this is the OBJECT, and the operator is told the resolved name rather than assuming
either — a hardcoded cluster-scoped name is a collision waiting for the second install.
*/}}
{{- define "crystal-backup.manifestMoverServiceAccountName" -}}
{{- default (printf "%s-manifest-mover" (include "crystal-backup.fullname" .)) .Values.manifestMover.serviceAccountName -}}
{{- end -}}

{{/*
The port(s) the Kubernetes API server answers on, as a comma-separated string. Consumers split
it and cast each element with `int` — a NetworkPolicy port given as a STRING is a named port, so
the cast is not cosmetic.

WHY THIS IS A LIST. It was a scalar defaulting to 443, and that default made the operator unable
to start on k3s, RKE2, kubeadm — most of the world:

  Failed to start manager: failed to get server groups: Get "https://10.43.0.1:443/api":
  dial tcp: i/o timeout

The `kubernetes` Service listens on 443 and DNATs to the API server's Endpoints, which on those
distributions are on 6443. kube-proxy rewrites the destination port BEFORE the CNI evaluates
egress, so a policy naming 443 never matches the packet that actually leaves the pod. There is no
way for the chart to know which of the two it will be, and guessing wrong costs an operator that
never starts — so the default is the SUPERSET and narrowing it is the deliberate act. The security
cost of one extra outbound TCP port, on a policy whose destination is already the API server, is
not comparable.

The crucible knew this before the chart did: test/crucible/deploy/deploy.sh carried
`--set networkPolicy.apiServerPort=6443` with a comment quoting that error verbatim, which is why
CI never saw the bug. The override is gone; the campaign now installs what a user installs.

`networkPolicy.apiServerPort` (scalar) is still accepted and REPLACES the list when set, so an
install that narrowed it to one port keeps exactly the posture it asked for.
*/}}
{{- define "crystal-backup.apiServerPorts" -}}
{{- $ports := .Values.networkPolicy.apiServerPorts | default list -}}
{{- with .Values.networkPolicy.apiServerPort -}}
{{- $ports = list . -}}
{{- end -}}
{{- $out := list -}}
{{- range $ports -}}
{{- $out = append $out (. | toString) -}}
{{- end -}}
{{- $out = uniq $out -}}
{{- if not $out -}}
{{- fail "networkPolicy.apiServerPorts is empty, so nothing in crystal-backup-system could reach the API server: the operator would fail its first discovery call with \"dial tcp: i/o timeout\" and the manifest mover would never capture a manifest. The default is [443, 6443] — 443 for a cluster whose apiserver Endpoints are themselves on 443, 6443 for k3s/RKE2/kubeadm, where kube-proxy DNATs the kubernetes Service to 6443 before the CNI evaluates egress. Narrow it to the port your cluster actually uses, or leave the default." -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{/*
Fully-resolved operator image reference. Prefers the immutable digest pin; falls
back to a tag (default: appVersion) only when no digest is configured.
*/}}
{{- define "crystal-backup.image" -}}
{{- $repo := .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/*
The fully-qualified mover image reference the operator passes to every mover Job via
--mover-image. Digest-pinned when mover.image.digest is set (production references the mover BY
DIGEST), else repository:tag with the tag defaulting to the chart appVersion. Mirrors
crystal-backup.image so the operator and mover images share one resolution rule.
*/}}
{{- define "crystal-backup.moverImage" -}}
{{- $repo := .Values.mover.image.repository -}}
{{- if .Values.mover.image.digest -}}
{{- printf "%s@%s" $repo .Values.mover.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.mover.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/*
The fully-qualified SYNC image reference, passed via --sync-image and used ONLY by external-sync
Jobs. Same resolution rule as the mover image; a separate image because sync additionally needs
rclone, and rclone has no business on the backup/restore path (adr/0013 amendment).

An operator with no external sync configured never pulls it, so leaving it at the placeholder
digest costs nothing until the first ClusterBackupExternalSync/BackupExternalSync exists.
*/}}
{{- define "crystal-backup.syncImage" -}}
{{- $repo := .Values.sync.image.repository -}}
{{- if .Values.sync.image.digest -}}
{{- printf "%s@%s" $repo .Values.sync.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.sync.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}
