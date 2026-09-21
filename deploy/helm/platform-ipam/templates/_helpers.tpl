{{- define "platform-ipam.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "platform-ipam.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "platform-ipam.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "platform-ipam.labels" -}}
app.kubernetes.io/name: {{ include "platform-ipam.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "platform-ipam.image" -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- end -}}

{{/* The application accepts one complete, validated policy document.  Keep
the Helm-only UI values in that document instead of exporting environment
variables that the configuration loader does not consume. */}}
{{- define "platform-ipam.policy" -}}
{{- $policy := deepCopy .Values.policy.data -}}
{{- $_ := set $policy "ui" (dict "inventory_links_enabled" .Values.ui.inventoryLinksEnabled "netbox_base_url" .Values.ui.netboxBaseURL) -}}
{{- toYaml $policy -}}
{{- end -}}

{{- define "platform-ipam.serviceAccountName" -}}
{{- $root := .root -}}
{{- $workload := .workload -}}
{{- if $workload.serviceAccount.create -}}
{{- default (printf "%s-%s" (include "platform-ipam.fullname" $root) .name) $workload.serviceAccount.name -}}
{{- else -}}
{{- $workload.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The environment api and worker share today, and that the operator Job
(templates/operator-job.yaml, work-plan package H3) reuses for mode: adopt --
adopt dispatches in cmd/platform-ipam/main.go exactly where worker does and
passes through the very same Settings.Validate("adopt"), which requires a
database URL and (in stage/prod) live-AWS settings regardless of mode, even
though adopt itself never starts an HTTP listener or authenticates a caller.

Takes a dict: {root: $, full: true|false, identity: true|false (optional,
default true)}. full: true (the api/worker Deployments, byte-identical to
before this helper was parameterized) renders every variable; full: false
(the operator Job for ALL THREE adopt subcommands -- plan, apply and abandon
alike, work-plan packages H5 and H7) renders the same set minus
IPAM_AUTH_MODE, IPAM_LISTEN_ADDR, IPAM_OIDC_ISSUER and IPAM_OIDC_AUDIENCE:
adopt authenticates no HTTP caller and never listens
(internal/config/config.go's Settings.Validate no longer demands these of it
either), so the Job never reads them. ONE definition of every variable's
name/value either way -- omitted for full: false, never a second copy -- so
the two callers cannot drift apart.

identity: false (work-plan package H9; only the operator Job's own
"abandon" command passes this) additionally omits IPAM_IDENTITY_FILE even
when identity.existingConfigMap is set chart-wide: `adopt abandon` resolves
no domain.Principal at all (internal/adoptcmd/abandon.go calls runAbandon
without cfg, and Service.AbandonAdoption takes no domain.Principal, ADR
0012) and Settings.Validate("adopt") never requires IPAM_IDENTITY_FILE
either, so mounting an identity file into this Job would be a credential the
process cannot use. Every other caller omits this key from the dict, which
defaults to true and reproduces today's rule (the var follows
identity.existingConfigMap alone). */}}
{{- define "platform-ipam.workloadEnv" -}}
{{- $root := .root -}}
{{- $full := .full -}}
{{- $identity := true -}}
{{- if hasKey . "identity" -}}
{{- $identity = .identity -}}
{{- end -}}
- name: IPAM_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ $root.Values.database.existingSecret }}
      key: {{ $root.Values.database.secretKey }}
- name: IPAM_CONFIG_FILE
  value: /etc/platform-ipam/config/pools.yaml
- name: IPAM_ENVIRONMENT
  value: {{ $root.Values.deploymentEnvironment | quote }}
{{- if $full }}
- name: IPAM_AUTH_MODE
  value: {{ $root.Values.auth.mode | quote }}
{{- end }}
- name: IPAM_NETBOX_URL
  value: {{ $root.Values.netbox.url | quote }}
- name: IPAM_NETBOX_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $root.Values.netbox.existingSecret }}
      key: {{ $root.Values.netbox.secretKey }}
- name: IPAM_AWS_MODE
  value: {{ $root.Values.aws.mode | quote }}
{{- if $full }}
- name: IPAM_LISTEN_ADDR
  value: ":8080"
- name: IPAM_OIDC_ISSUER
  value: {{ $root.Values.auth.issuer | quote }}
- name: IPAM_OIDC_AUDIENCE
  value: {{ $root.Values.auth.audience | quote }}
{{- end }}
{{- if and $identity $root.Values.identity.existingConfigMap }}
- name: IPAM_IDENTITY_FILE
  value: /etc/platform-ipam/identity/identities.yaml
{{- end }}
{{- end -}}

{{/* Shared volume/volumeMount fragments for the policy ConfigMap, the
optional identity ConfigMap, and a writable /tmp under a read-only root
filesystem -- used by the api/worker Deployments and (for the "policy" and
"tmp" pair always, "identity" only when mode: adopt) the operator Job.
Takes the root context. */}}
{{- define "platform-ipam.policyVolumeMount" -}}
- name: policy
  mountPath: /etc/platform-ipam/config
  readOnly: true
{{- end -}}

{{- define "platform-ipam.policyVolume" -}}
- name: policy
  configMap:
    name: {{ .Values.policy.configMapName }}
{{- end -}}

{{/* Unconditional, like every other helper in this file -- the caller
guards with {{- if .Values.identity.existingConfigMap }} around the include,
exactly as the original hand-written deployment.yaml did. A self-guarding
version that a caller piped through `nindent` unconditionally was tried and
reverted: Sprig's nindent prepends a newline+indent even to an empty string,
so it left a whitespace-only line in the rendered output whenever
identity.existingConfigMap was unset -- breaking the byte-identical-when-
disabled property work-plan package H3 requires. */}}
{{- define "platform-ipam.identityVolumeMount" -}}
- name: identity
  mountPath: /etc/platform-ipam/identity
  readOnly: true
{{- end -}}

{{- define "platform-ipam.identityVolume" -}}
- name: identity
  configMap:
    name: {{ .Values.identity.existingConfigMap }}
{{- end -}}

{{- define "platform-ipam.tmpVolumeMount" -}}
- name: tmp
  mountPath: /tmp
{{- end -}}

{{- define "platform-ipam.tmpVolume" -}}
- name: tmp
  emptyDir: {}
{{- end -}}

{{- define "platform-ipam.uiProxy.image" -}}
{{- if .Values.uiProxy.image.digest -}}
{{ .Values.uiProxy.image.repository }}@{{ .Values.uiProxy.image.digest }}
{{- else -}}
{{ .Values.uiProxy.image.repository }}:{{ .Values.uiProxy.image.tag }}
{{- end -}}
{{- end -}}

{{/* ui-proxy's Caddyfile: the only published path to an external NetBox UI
(ADR 0006, docs/GUI_AUTHENTICATION.md section 2). This reproduces
deploy/compose/ui-proxy/Caddyfile's logic exactly -- same ordered `route`
block, same identity-header strip in both spellings, same 403 for /api/ and
/graphql/, same bcrypt basic_auth, same trusted X-Remote-User -- with the
upstream templated from .Values.uiProxy.upstream. Unlike Compose there is no
hash-at-start entrypoint: NETBOX_UI_PASSWORD_HASH is already a bcrypt hash,
read straight from .Values.uiProxy.basicAuth.existingSecret via
secretKeyRef. */}}
{{- define "platform-ipam.uiProxy.caddyfile" -}}
{
	admin off
	auto_https off
}

:8080 {
	# A single `route` block so these steps run in exactly the order written --
	# Caddy's default directive ordering must not be relied on for a
	# security-sensitive sequence like this one.
	route {
		# Unauthenticated healthcheck target for the liveness/readiness probes
		# below. It never reaches NetBox and needs no credential.
		@health path /healthz
		respond @health 200

		# Rule 2: delete any client-supplied identity header BEFORE
		# authentication ever runs, so a spoofed header can never reach
		# NetBox, authenticated or not.
		request_header -X-Remote-User
		request_header -X-Remote-User-Group
		# The same names spelled with underscores. A WSGI server builds
		# HTTP_X_REMOTE_USER by upper-casing the name and turning dashes into
		# underscores, so "X_Remote_User" lands in the very same variable and
		# would slip past a filter that only knows the dashed spelling.
		request_header -X_Remote_User
		request_header -X_Remote_User_Group

		# Rule 3: this proxy serves browsers only. NetBox's REST and GraphQL
		# API must stay reachable solely on the internal network -- checked
		# before authentication so an API caller gets a plain 403, not a
		# login prompt.
		@blocked path /api/* /graphql/*
		respond @blocked 403

		# Matrix-parameter spelling of the same paths ("/api;x=1/..."), which the
		# glob above does not match. Kept identical to the Compose Caddyfile.
		@blocked_params path_regexp (?i)^/+(api|graphql);
		respond @blocked_params 403

		# Verifies the Basic credential against the bcrypt hash. No
		# credential, or a wrong one, gets a 401 with WWW-Authenticate here --
		# the request goes no further.
		basic_auth bcrypt {
			{$NETBOX_UI_USER} {$NETBOX_UI_PASSWORD_HASH}
		}

		# Only after authentication succeeds does NetBox learn who the user
		# is, from the name the proxy itself just verified -- never from
		# anything the client sent.
		request_header X-Remote-User {http.auth.user.id}

		reverse_proxy {{ .Values.uiProxy.upstream }}
	}
}
{{- end -}}
