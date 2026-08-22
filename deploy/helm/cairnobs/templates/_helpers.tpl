{{/*
Standard labels applied to every resource this chart renders.
*/}}
{{- define "cairnobs.labels" -}}
app.kubernetes.io/part-of: cairnobs
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/*
Per-component selector labels -- usage:
{{ include "cairnobs.selectorLabels" (list $ "api") }}
A plain string arg (the old shape this started with) can't reach
$.Release from inside the defined template -- `include`'s argument
becomes the template's entire root context, so a bare "api" string
leaves no way to get back to the chart root. A two-element list carries
both.
*/}}
{{- define "cairnobs.selectorLabels" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
app.kubernetes.io/name: cairnobs-{{ $name }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
{{- end -}}

{{/*
An initContainer that busy-waits for a TCP host:port to accept
connections -- usage: {{ include "cairnobs.waitForTCP" (list "name-suffix" "host" "port") }}

This approximates docker-compose.yml's `depends_on: condition:
service_healthy` (waits for the dependency's process to be reachable),
but NOT `condition: service_completed_successfully` (waits for a
one-shot Job, like clickhouse-migrate, to have actually finished). That
second guarantee doesn't have a lightweight equivalent here without
giving every app pod's ServiceAccount RBAC to read Job status, which is
a lot of privilege for a startup-ordering nicety -- see
deploy/helm/cairnobs/README.md's "Startup ordering" section. The gap it
leaves (a pod starts before its migration Job has finished) is covered
by the app's own crash-and-restart-on-connect/schema failure: every Go
service here already os.Exit(1)s on a failed Postgres/ClickHouse ping at
startup (see e.g. api/cmd/api/main.go), so Kubernetes' restart policy
naturally retries until the schema is ready. Documented as a real,
accepted tradeoff, not implied to be a hard ordering guarantee.
*/}}
{{- define "cairnobs.waitForTCP" -}}
{{- $name := index . 0 -}}
{{- $host := index . 1 -}}
{{- $port := index . 2 -}}
- name: wait-for-{{ $name }}
  image: busybox:1.36
  command:
    - sh
    - -c
    - until nc -z -w2 {{ $host }} {{ $port }}; do echo "waiting for {{ $host }}:{{ $port }}"; sleep 2; done
{{- end -}}
