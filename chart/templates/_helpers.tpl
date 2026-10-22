{{/* Release charts pin each image by digest. Development builds retain latest
so Skaffold can replace bare image names with the images it built. */}}
{{- define "agent.image" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{- $image := $name -}}
{{- with $root.Values.imageRepository -}}
{{- $image = printf "%s/%s" . $name -}}
{{- end -}}
{{- with (index $root.Values.imageDigests $name) -}}
{{- printf "%s@%s" $image . -}}
{{- else -}}
{{- printf "%s:latest" $image -}}
{{- end -}}
{{- end -}}

{{/* RabbitmqCluster name; its topology objects share the prefix before -events. */}}
{{- define "agent.rabbitmq" -}}
{{- .Values.rabbitmq.name | default (printf "%s-events" .Release.Name) -}}
{{- end -}}
{{- define "agent.rabbitmqPrefix" -}}
{{- trimSuffix "-events" (include "agent.rabbitmq" .) -}}
{{- end -}}
