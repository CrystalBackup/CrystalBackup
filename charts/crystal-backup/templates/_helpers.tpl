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
