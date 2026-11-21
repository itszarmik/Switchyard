{{/*
Names. The full name is <release>-<chart name>, or the release name alone
when it already contains the chart name, truncated to the 63 characters a
DNS label allows.
*/}}
{{- define "switchyard.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "switchyard.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "switchyard.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels. Every object gets the common labels; selectors use only the
name and instance, which never change across upgrades.
*/}}
{{- define "switchyard.selectorLabels" -}}
app.kubernetes.io/name: {{ include "switchyard.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "switchyard.labels" -}}
helm.sh/chart: {{ include "switchyard.chart" . }}
{{ include "switchyard.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: switchyard
{{- end }}

{{/* The gateway pods' selector: the selector labels plus the component. */}}
{{- define "switchyard.gatewaySelectorLabels" -}}
{{ include "switchyard.selectorLabels" . }}
app.kubernetes.io/component: gateway
{{- end }}

{{- define "switchyard.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "switchyard.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The Secret the pods load with envFrom. */}}
{{- define "switchyard.secretName" -}}
{{- if .Values.secrets.create }}
{{- include "switchyard.fullname" . }}
{{- else }}
{{- .Values.secrets.existingSecret }}
{{- end }}
{{- end }}

{{/* The image reference: repository:tag, or repository:tag@digest. */}}
{{- define "switchyard.image" -}}
{{- $tag := default (printf "v%s" .Chart.AppVersion) .Values.image.tag }}
{{- if .Values.image.digest }}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}

{{/*
switchyard.merge deep-merges src into dst in place: (list dst src). Maps
merge key by key; any other src value, a list or false or "" included,
replaces dst's; a null src value removes the key. Sprig's mergeOverwrite is
not used because it skips false, 0 and "" in src, so `enabled: false`
could not turn a default off.
*/}}
{{- define "switchyard.merge" -}}
{{- $dst := index . 0 }}
{{- range $k, $v := index . 1 }}
{{- if kindIs "invalid" $v }}
{{- $_ := unset $dst $k }}
{{- else if and (kindIs "map" $v) (kindIs "map" (get $dst $k)) }}
{{- include "switchyard.merge" (list (get $dst $k) $v) }}
{{- else }}
{{- $_ := set $dst $k $v }}
{{- end }}
{{- end }}
{{- end }}

{{/*
switchyard.config is the rendered Switchyard config, as YAML: the `config`
value merged over files/switchyard.yaml. It also checks what the chart
relies on: the listener ports and a catalog for the default include.
*/}}
{{- define "switchyard.config" -}}
{{- $cfg := .Files.Get "files/switchyard.yaml" | fromYaml }}
{{- include "switchyard.merge" (list $cfg (deepCopy (.Values.config | default dict))) }}
{{- $server := get $cfg "server" | default dict }}
{{- $listen := toString (get $server "listen" | default ":8080") }}
{{- $admin := toString (get $server "admin_listen" | default "") }}
{{- if not (hasSuffix ":8080" $listen) }}
{{- fail (printf "config.server.listen is %q: the chart's container port, Service and test use 8080, so keep the port (for example \":8080\")" $listen) }}
{{- end }}
{{- if or (not (hasSuffix ":9090" $admin)) (regexMatch "^(127\\.|localhost:|\\[::1\\]:)" $admin) }}
{{- fail (printf "config.server.admin_listen is %q: the probes and metrics use port 9090 on the pod IP, so it must be \":9090\" (all interfaces; the admin token guards /admin/v1/*)" $admin) }}
{{- end }}
{{- if and (not (include "switchyard.hasCatalog" .)) (has "../catalog/*.yaml" (get $cfg "include" | default list)) }}
{{- fail "catalog.bundled is false and catalog.extraFiles is empty, but config.include still loads ../catalog/*.yaml: add catalog files, or override config.include (and config.models)" }}
{{- end }}
{{- include "switchyard.checkNoLiteralSecrets" $cfg }}
{{- toYaml $cfg }}
{{- end }}

{{/*
switchyard.checkNoLiteralSecrets stops rendering when a secret field of
the merged config (the gateway's Secret-typed fields) holds a literal
value: the config is a ConfigMap, which anyone who may read ConfigMaps in
the namespace can read. Each must be exactly one reference: ${VAR},
${VAR:-default} or ${file:PATH}. The tracing endpoint is usually a plain
URL, so only a password in it is refused. The messages name the field,
never its value. Called with the merged config.
*/}}
{{- define "switchyard.checkNoLiteralSecrets" -}}
{{- $fields := dict
  "server.admin_token" (dig "server" "admin_token" "" .)
  "storage.redis.cache_url" (dig "storage" "redis" "cache_url" "" .)
  "storage.redis.state_url" (dig "storage" "redis" "state_url" "" .)
  "storage.ledger.dsn" (dig "storage" "ledger" "dsn" "" .) }}
{{- range $i, $p := get . "providers" | default list }}
{{- $_ := set $fields (printf "providers[%d].api_key (provider %s)" $i (toString (get $p "id"))) (get $p "api_key") }}
{{- end }}
{{- range $path := keys $fields | sortAlpha }}
{{- $v := get $fields $path }}
{{- if and (not (kindIs "invalid" $v)) (toString $v) (not (regexMatch "^\\$\\{([A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?|file:[^}]+)\\}$" (toString $v))) }}
{{- fail (printf "config.%s is a literal value: write it as one reference to a Secret key, such as ${OPENAI_API_KEY}, and put the value in secrets.env (or your existingSecret). The config is a ConfigMap, readable by anyone who can read ConfigMaps in the namespace" $path) }}
{{- end }}
{{- end }}
{{- if regexMatch "//[^/@]*:[^/@]+@" (toString (dig "telemetry" "tracing" "endpoint" "" .)) }}
{{- fail "config.telemetry.tracing.endpoint has a literal password: write the whole endpoint as one ${VAR} reference to a Secret key" }}
{{- end }}
{{- end }}

{{/* Non-empty when the catalog ConfigMap has at least one file. */}}
{{- define "switchyard.hasCatalog" -}}
{{- if or .Values.catalog.bundled .Values.catalog.extraFiles }}true{{ end }}
{{- end }}

{{/*
The catalog files, name: content: the bundled ones (unless
catalog.bundled is false) with catalog.extraFiles over them.
*/}}
{{- define "switchyard.catalogFiles" -}}
{{- $files := dict }}
{{- if .Values.catalog.bundled }}
{{- range $path, $_ := .Files.Glob "files/catalog/*.yaml" }}
{{- $_ := set $files (base $path) ($.Files.Get $path) }}
{{- end }}
{{- end }}
{{- range $name, $content := .Values.catalog.extraFiles }}
{{- $_ := set $files $name $content }}
{{- end }}
{{- toYaml $files }}
{{- end }}

{{/*
switchyard.seconds converts a duration from the config to whole seconds:
"0s", "90s", "2m", "1m30s", "1h", "1h2m3s". Anything else (fractions, ms,
${VAR} references, and bare numbers, which the gateway rejects) stops
rendering and asks for an explicit terminationGracePeriodSeconds. Called
with (list key value).
*/}}
{{- define "switchyard.seconds" -}}
{{- $key := index . 0 }}
{{- $d := toString (index . 1) }}
{{- if and $d (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $d) }}
{{- $h := regexFind "[0-9]+h" $d | trimSuffix "h" | default "0" | atoi }}
{{- $m := regexFind "[0-9]+m" $d | trimSuffix "m" | default "0" | atoi }}
{{- $s := regexFind "[0-9]+s" $d | trimSuffix "s" | default "0" | atoi }}
{{- add (mul $h 3600) (mul $m 60) $s }}
{{- else }}
{{- fail (printf "cannot derive terminationGracePeriodSeconds from %s %q: the chart understands only whole h, m and s (for example 90s or 1m30s). Set terminationGracePeriodSeconds to shutdown_delay + shutdown_grace + 15 seconds" $key $d) }}
{{- end }}
{{- end }}

{{/*
switchyard.terminationGracePeriodSeconds: the value, or shutdown_delay +
shutdown_grace + 15 from the rendered config (DESIGN §14.8: the worst case
from SIGTERM to exit). It mirrors the gateway's defaults
(internal/config/defaults.go): an unset delay is 0s, and an unset or zero
grace is 60s.
*/}}
{{- define "switchyard.terminationGracePeriodSeconds" -}}
{{- if not (kindIs "invalid" .Values.terminationGracePeriodSeconds) }}
{{- .Values.terminationGracePeriodSeconds }}
{{- else }}
{{- $server := get (include "switchyard.config" . | fromYaml) "server" | default dict }}
{{- $delay := include "switchyard.seconds" (list "server.shutdown_delay" (get $server "shutdown_delay" | default "0s")) | atoi }}
{{- $grace := include "switchyard.seconds" (list "server.shutdown_grace" (get $server "shutdown_grace" | default "60s")) | atoi }}
{{- add $delay ($grace | default 60) 15 }}
{{- end }}
{{- end }}

{{/*
switchyard.checkSecrets stops rendering when the chart-created Secret
misses a required key or has an admin token the gateway would refuse, or
for a ${VAR} reference without a default that no Secret key or extraEnv
entry provides. Its messages name keys, never values. With an existingSecret, or
extraEnvFrom, the chart cannot see the keys, and the gateway reports a
missing variable at startup instead.
*/}}
{{- define "switchyard.checkSecrets" -}}
{{- if .Values.secrets.create }}
{{- if .Values.secrets.existingSecret }}
{{- fail "secrets.existingSecret is set but secrets.create is true: set secrets.create=false to use your Secret, or remove existingSecret" }}
{{- end }}
{{- $env := .Values.secrets.env | default dict }}
{{- $missing := list }}
{{- range list "SWITCHYARD_ADMIN_TOKEN" "DATABASE_URL" "REDIS_CACHE_URL" "REDIS_STATE_URL" }}
{{- if not (get $env .) }}
{{- $missing = append $missing . }}
{{- end }}
{{- end }}
{{- if $missing }}
{{- fail (printf "secrets.env is missing %s: set them (for example -f secrets.yaml), or set secrets.create=false and secrets.existingSecret to a Secret that has them" (join ", " $missing)) }}
{{- end }}
{{- /* Here, not in the schema: a schema pattern's error quotes the value. */}}
{{- $token := toString (get $env "SWITCHYARD_ADMIN_TOKEN") }}
{{- if regexMatch "[\\p{Z}\\p{Cc}]" $token }}
{{- fail "secrets.env.SWITCHYARD_ADMIN_TOKEN contains whitespace or a control character, often a trailing newline from --set-file or a YAML | block: remove it" }}
{{- end }}
{{- if lt (len $token) 32 }}
{{- fail "secrets.env.SWITCHYARD_ADMIN_TOKEN is shorter than 32 characters: make one with openssl rand -base64 32" }}
{{- end }}
{{- if not .Values.extraEnvFrom }}
{{- $have := dict }}
{{- range .Values.extraEnv }}{{ $_ := set $have .name true }}{{ end }}
{{- $unset := list }}
{{- /* As the gateway reads it, $$ is a literal $, so ${VAR} is a reference
       only after an even number of $: $${VAR} is literal text. */}}
{{- range regexFindAll "\\$+\\{[A-Za-z_][A-Za-z0-9_]*\\}" (include "switchyard.config" .) -1 }}
{{- $var := regexFind "[A-Za-z_][A-Za-z0-9_]*" . }}
{{- if and (eq (mod (len (regexFind "^\\$+" .)) 2) 1) (not (hasKey $env $var)) (not (hasKey $have $var)) (not (has $var $unset)) }}
{{- $unset = append $unset $var }}
{{- end }}
{{- end }}
{{- if $unset }}
{{- fail (printf "the config refers to %s without a default, but secrets.env does not set it: add it to secrets.env (or extraEnv, if it is not a secret)" (join ", " $unset)) }}
{{- end }}
{{- end }}
{{- else if not .Values.secrets.existingSecret }}
{{- fail "secrets.create is false, so secrets.existingSecret must name the Secret with SWITCHYARD_ADMIN_TOKEN, DATABASE_URL, REDIS_CACHE_URL and REDIS_STATE_URL" }}
{{- end }}
{{- end }}
