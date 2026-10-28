{{/*
Application listening on port 80 exposed via an HTTPS URL: Deployment + Service + IngressRoute
Usage: {{ include "public-url-application" . }}

Values: id and image (set by the pipeline); url, or domain (set by the
pipeline) to publish at <id>.<domain>; optional replicaCount and pathPrefix.
Pods pull with their namespace's default service account, which carries the
registry pull secret.

Environments: the pipelines also set environment (the environment being
deployed to) and productionEnvironment (the stack's first environment). The
host is chosen like this:
- environment empty, productionEnvironment empty, or the two equal (a
  production deploy): url, else <id>.<domain>.
- any other environment: environmentUrls.<environment> when the chart sets
  it, else <id>-<environment>.<domain>, which the stack's wildcard DNS record
  and certificate already cover. url is ignored there: it names the
  production host.
Only strings and numbers count (--set reads 123 as a number); any other
value, such as a map a chart keeps under environment for its own use, counts
as unset and keeps the production rule.
*/}}
{{- define "public-url-application" -}}
{{- $environment := "" -}}
{{- if or (kindIs "string" .Values.environment) (kindIs "int64" .Values.environment) (kindIs "float64" .Values.environment) -}}
{{- $environment = toString .Values.environment -}}
{{- end -}}
{{- $production := "" -}}
{{- if or (kindIs "string" .Values.productionEnvironment) (kindIs "int64" .Values.productionEnvironment) (kindIs "float64" .Values.productionEnvironment) -}}
{{- $production = toString .Values.productionEnvironment -}}
{{- end -}}
{{- $url := .Values.url -}}
{{- if and $environment $production (ne $environment $production) -}}
{{- $url = "" -}}
{{- $environmentUrls := .Values.environmentUrls -}}
{{- if kindIs "map" $environmentUrls -}}
{{- $url = index $environmentUrls $environment | default "" -}}
{{- end -}}
{{- if not $url -}}
{{- $url = printf "%s-%s.%s" (toString .Values.id) $environment (required (printf "environmentUrls.%s or domain is required" $environment) .Values.domain) -}}
{{- end -}}
{{- else if not $url -}}
{{- $url = printf "%s.%s" .Values.id (required "url or domain is required" .Values.domain) -}}
{{- end -}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Values.id }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: {{ .Values.replicaCount | default 1 }}
  selector:
    matchLabels:
      app: {{ .Values.id }}
  template:
    metadata:
      labels:
        app: {{ .Values.id }}
    spec:
      containers:
        - name: {{ .Values.id }}
          image: {{ .Values.image }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.id }}
  namespace: {{ .Release.Namespace }}
spec:
  type: ClusterIP
  selector:
    app: {{ .Values.id }}
  ports:
    - port: 80
      targetPort: 80
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: {{ .Values.id }}
  namespace: {{ .Release.Namespace }}
spec:
  entryPoints:
    - websecure
  routes:
    - kind: Rule
      match: Host(`{{ $url }}`){{- if .Values.pathPrefix }} && ( Path(`{{ .Values.pathPrefix }}`) || PathPrefix(`{{ .Values.pathPrefix }}/`) ){{- end }}
      services:
        - name: {{ .Values.id }}
          port: 80
  tls:
    certResolver: main
{{- end -}}
