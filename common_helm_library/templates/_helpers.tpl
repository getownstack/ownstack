{{/*
Application listening on port 80 exposed via an HTTPS URL: Deployment + Service + IngressRoute
Usage: {{ include "public-url-application" . }}

Values: id and image (set by the pipeline); url, or domain (set by the
pipeline) to publish at <id>.<domain>; optional replicaCount, pathPrefix,
and imagePullSecret (set by the pipeline from the registry contract).
*/}}
{{- define "public-url-application" -}}
{{- $url := .Values.url -}}
{{- if not $url -}}
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
      imagePullSecrets:
        - name: {{ .Values.imagePullSecret | default "harbor-robot" }}
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
