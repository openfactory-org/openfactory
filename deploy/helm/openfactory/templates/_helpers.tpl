{{/* Chart name. */}}
{{- define "openfactory.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "openfactory.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "openfactory.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "openfactory.labels" -}}
helm.sh/chart: {{ include "openfactory.chart" . }}
app.kubernetes.io/name: {{ include "openfactory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "openfactory.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openfactory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "openfactory.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "openfactory.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "openfactory.workerServiceAccountName" -}}
{{- printf "%s-worker" (include "openfactory.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "openfactory.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "openfactory.secretName" -}}
{{- default (include "openfactory.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{/* Image of the runner binary: runner.image, or the server image. */}}
{{- define "openfactory.runnerImage" -}}
{{- default (include "openfactory.image" .) .Values.runner.image -}}
{{- end -}}

{{/* True when the install creates the master key with the hook Job: the
     server is installed and neither secretKey nor existingSecret is set. */}}
{{- define "openfactory.generateKey" -}}
{{- if and .Values.server.enabled (not .Values.secretKey) (not .Values.existingSecret) -}}true{{- end -}}
{{- end -}}

{{/* Name of the Secret that the hook Job creates for the master key. */}}
{{- define "openfactory.keySecretName" -}}
{{- printf "%s-key" (include "openfactory.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "openfactory.sandboxNamespace" -}}
{{- default .Release.Namespace .Values.sandbox.namespace -}}
{{- end -}}

{{/* URL that sandbox pods use to reach the server (the server Service by default). */}}
{{- define "openfactory.internalURL" -}}
{{- if .Values.server.internalURL -}}
{{- .Values.server.internalURL | trimSuffix "/" -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%v" (include "openfactory.fullname" .) .Release.Namespace .Values.server.service.port -}}
{{- end -}}
{{- end -}}

{{/* External URL of the server. */}}
{{- define "openfactory.publicURL" -}}
{{- if .Values.server.publicURL -}}
{{- .Values.server.publicURL | trimSuffix "/" -}}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts -}}
{{- $host := (index .Values.ingress.hosts 0).host -}}
{{- if .Values.ingress.tls -}}https://{{ $host }}{{- else -}}http://{{ $host }}{{- end -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%v" (include "openfactory.fullname" .) .Release.Namespace .Values.server.service.port -}}
{{- end -}}
{{- end -}}

{{/* Database URL when it is rendered into the chart Secret. Empty for SQLite
     and for an external URL read from database.postgres.existingSecret. */}}
{{- define "openfactory.databaseURL" -}}
{{- if eq .Values.database.mode "postgres" -}}
{{- if not .Values.database.postgres.existingSecret -}}
{{- .Values.database.postgres.url -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* True when the server or the worker uses the kubernetes backend. */}}
{{- define "openfactory.needsSandboxRBAC" -}}
{{- if and .Values.sandbox.rbac.create (or (and .Values.server.enabled (eq .Values.server.sandboxBackend "kubernetes")) .Values.worker.enabled) -}}true{{- end -}}
{{- end -}}

{{/* Workload kind of the server: StatefulSet (sqlite) or Deployment (postgres). */}}
{{- define "openfactory.serverKind" -}}
{{- if eq .Values.database.mode "sqlite" -}}StatefulSet{{- else -}}Deployment{{- end -}}
{{- end -}}

{{/* Rejects unsupported value combinations. Rendered by the server workload. */}}
{{- define "openfactory.validate" -}}
{{- if and (not .Values.server.enabled) (not .Values.worker.enabled) -}}
{{- fail "nothing to install: enable server.enabled, worker.enabled or both" -}}
{{- end -}}
{{- if and (not .Values.server.enabled) (not .Values.worker.serverURL) -}}
{{- fail "worker.serverURL is required when server.enabled is false" -}}
{{- end -}}
{{- if .Values.server.enabled -}}
{{- include "openfactory.validateServer" . -}}
{{- end -}}
{{- end -}}

{{- define "openfactory.validateServer" -}}
{{- $mode := .Values.database.mode -}}
{{- if not (has $mode (list "sqlite" "postgres")) -}}
{{- fail (printf "database.mode %q: want sqlite or postgres" $mode) -}}
{{- end -}}
{{- if eq .Values.server.sandboxBackend "local" -}}
{{- fail "server.sandboxBackend local: the chart does not accept it, because the local backend runs agents in the server Pod without isolation. Use kubernetes. For development, run openfactory serve with OF_DEV=true on a workstation" -}}
{{- end -}}
{{- if ne .Values.server.sandboxBackend "kubernetes" -}}
{{- fail (printf "server.sandboxBackend %q: the only value is kubernetes" .Values.server.sandboxBackend) -}}
{{- end -}}
{{- if lt (int .Values.server.replicaCount) 1 -}}
{{- fail "server.replicaCount must be at least 1" -}}
{{- end -}}
{{- if eq $mode "sqlite" -}}
{{- if gt (int .Values.server.replicaCount) 1 -}}
{{- fail (printf "server.replicaCount is %v but database.mode sqlite supports exactly one replica; use database.mode postgres to run more" .Values.server.replicaCount) -}}
{{- end -}}
{{- else -}}
{{- if not (or .Values.database.postgres.url .Values.database.postgres.existingSecret .Values.existingSecret) -}}
{{- fail "database.mode postgres needs database.postgres.url or database.postgres.existingSecret" -}}
{{- end -}}
{{- if and (gt (int .Values.server.replicaCount) 1) (not .Values.objectStore.url) -}}
{{- fail "objectStore.url is required with more than one replica: set s3://, gs:// or azblob:// storage, because replicas do not share a local disk" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.oidc.enabled (not (and .Values.oidc.issuer .Values.oidc.clientID)) -}}
{{- fail "oidc.enabled requires oidc.issuer and oidc.clientID" -}}
{{- end -}}
{{- end -}}

{{/* Comma-separated key=value list from a map (keys are sorted). */}}
{{- define "openfactory.kvList" -}}
{{- $first := true -}}
{{- range $k, $v := . -}}
{{- if not $first }},{{ end -}}
{{ $k }}={{ $v }}
{{- $first = false -}}
{{- end -}}
{{- end -}}

{{/* Entries "name=VAR1,VAR2" separated by semicolons (OF_PLUGIN_ENV). */}}
{{- define "openfactory.pluginEnv" -}}
{{- $first := true -}}
{{- range $name, $vars := . -}}
{{- if not $first }};{{ end -}}
{{ $name }}={{ include "openfactory.csv" $vars }}
{{- $first = false -}}
{{- end -}}
{{- end -}}

{{/* Comma-separated list from a list of strings. */}}
{{- define "openfactory.csv" -}}
{{- range $i, $v := . -}}
{{- if $i }},{{ end -}}
{{ $v }}
{{- end -}}
{{- end -}}

{{/* Control-plane Pods that a restricted sandbox can reach, as YAML with the
     keys namespace and selector. sandbox.k8s.controlPlane.selector wins. If it
     is empty, the server is installed and server.internalURL is empty (the
     sandboxes dial the server Service of this release), the selector is the
     labels of the server Pods and the namespace is the release namespace. The
     NetworkPolicy rule of a selector allows every port of those Pods, so the
     rule matches the container port after the Service translates port 80. */}}
{{- define "openfactory.controlPlanePeer" -}}
{{- $cp := .Values.sandbox.k8s.controlPlane | default dict -}}
{{- $selector := $cp.selector | default "" -}}
{{- $namespace := $cp.namespace | default "" -}}
{{- if and (not $selector) .Values.server.enabled (not .Values.server.internalURL) -}}
{{- $selector = printf "app.kubernetes.io/component=server,app.kubernetes.io/instance=%s,app.kubernetes.io/name=%s" .Release.Name (include "openfactory.name" .) -}}
{{- $namespace = $namespace | default .Release.Namespace -}}
{{- end -}}
namespace: {{ $namespace | quote }}
selector: {{ $selector | quote }}
{{- end -}}

{{/* Settings of the Kubernetes sandbox backend from sandbox.k8s, as "KEY: value"
     lines. The server ConfigMap and the worker Deployment both use it, so the
     two always get the same settings. */}}
{{- define "openfactory.sandboxK8sEnv" -}}
{{- with .Values.sandbox.k8s }}
{{- with .labels }}
OF_K8S_LABELS: {{ include "openfactory.kvList" . | quote }}
{{- end }}
{{- with .annotations }}
OF_K8S_ANNOTATIONS: {{ include "openfactory.kvList" . | quote }}
{{- end }}
{{- with .imagePullPolicy }}
OF_K8S_IMAGE_PULL_POLICY: {{ . | quote }}
{{- end }}
{{- with .runAsUser }}
OF_K8S_RUN_AS_USER: {{ . | quote }}
{{- end }}
{{- with .workspaceSizeLimit }}
OF_K8S_WORKSPACE_SIZE_LIMIT: {{ . | quote }}
{{- end }}
{{- if .preflightJob }}
OF_K8S_PREFLIGHT_JOB: "true"
{{- end }}
{{- with include "openfactory.controlPlanePeer" $ | fromYaml }}
{{- with .namespace }}
OF_K8S_CONTROL_PLANE_NAMESPACE: {{ . | quote }}
{{- end }}
{{- with .selector }}
OF_K8S_CONTROL_PLANE_SELECTOR: {{ . | quote }}
{{- end }}
{{- end }}
{{- with .egressProxy }}
OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE: {{ default (include "openfactory.runnerImage" $) .sidecarImage | quote }}
{{- with .lock }}
OF_K8S_EGRESS_LOCK: {{ . | quote }}
{{- end }}
{{- end }}
{{- with .networkPolicy }}
{{- with .check }}
OF_K8S_NETWORK_POLICY_CHECK: {{ . | quote }}
{{- end }}
{{- with .enforcement }}
OF_K8S_NETWORK_POLICY_ENFORCEMENT: {{ . | quote }}
{{- end }}
{{- with .waitTimeout }}
OF_K8S_EGRESS_WAIT_TIMEOUT: {{ . | quote }}
{{- end }}
{{- with .waitAddr }}
OF_K8S_EGRESS_WAIT_ADDR: {{ . | quote }}
{{- end }}
{{- end }}
{{- if .allowUnenforcedNetworkPolicy }}
OF_ALLOW_UNENFORCED_NETWORK_POLICY: "true"
{{- end }}
{{- end }}
{{- end -}}
