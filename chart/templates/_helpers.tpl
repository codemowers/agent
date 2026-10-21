{{/* Image reference: imageRepository prefix, fixed name, latest tag. Skaffold
replaces the bare names with the images it built. */}}
{{- define "agent.image" -}}
{{- $root := index . 0 -}}
{{- with $root.Values.imageRepository }}{{ . }}/{{ end }}{{ index . 1 }}:latest
{{- end -}}

{{/* RabbitmqCluster name; its topology objects share the prefix before -events. */}}
{{- define "agent.rabbitmq" -}}
{{- .Values.rabbitmq.name | default (printf "%s-events" .Release.Name) -}}
{{- end -}}
{{- define "agent.rabbitmqPrefix" -}}
{{- trimSuffix "-events" (include "agent.rabbitmq" .) -}}
{{- end -}}
