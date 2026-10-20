{{/* RabbitmqCluster name; its topology objects share the prefix before -events. */}}
{{- define "agent.rabbitmq" -}}
{{- .Values.rabbitmq.name | default (printf "%s-events" .Release.Name) -}}
{{- end -}}
{{- define "agent.rabbitmqPrefix" -}}
{{- trimSuffix "-events" (include "agent.rabbitmq" .) -}}
{{- end -}}
