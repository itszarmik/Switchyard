package helm

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.yaml.in/yaml/v3"

	"switchyard/internal/config"
)

// requiredEnv is a valid secrets.env: the four keys the chart requires,
// and the default tenant's key hash, which the default config references
// without a default, with throwaway values.
func requiredEnv() map[string]any {
	return map[string]any{
		"SWITCHYARD_API_KEY_HASH": "sha256:" + strings.Repeat("0", 64),
		"SWITCHYARD_ADMIN_TOKEN":  "render-test-admin-token-0123456789abcdef",
		"DATABASE_URL":            "postgres://switchyard:render-test@postgres:5432/switchyard?sslmode=disable",
		"REDIS_CACHE_URL":         "redis://redis-cache:6379/0",
		"REDIS_STATE_URL":         "redis://redis-state:6379/0",
	}
}

// minimalValues writes the smallest valid values: the required secrets.
func minimalValues(t *testing.T) string {
	t.Helper()
	return writeValues(t, map[string]any{"secrets": map[string]any{"env": requiredEnv()}})
}

// exampleValues are the chart's example values files, ci/*-values.yaml.
func exampleValues(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(chartDir, "ci", "*-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("found %d ci/*-values.yaml files in the chart, want at least default and full", len(files))
	}
	return files
}

// TestRenderSecretsOnlyInSecrets renders the chart with a sentinel value
// for every secret the chart knows, plus one that only the user's config
// references, over each example values file, and checks that the
// sentinels reach the pods only through a Secret: never a ConfigMap, the
// Deployment, the test hook or anything else (PLAN P9 acceptance 6).
func TestRenderSecretsOnlyInSecrets(t *testing.T) {
	requireHelm(t)
	keys := []string{
		"SWITCHYARD_ADMIN_TOKEN", "DATABASE_URL", "REDIS_CACHE_URL", "REDIS_STATE_URL",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "TOGETHER_API_KEY",
		"SWITCHYARD_API_KEY_HASH", "EXTRA_PROVIDER_KEY",
	}
	env := map[string]any{}
	sentinels := map[string]string{} // key -> the marker inside its value
	for i, k := range keys {
		marker := fmt.Sprintf("SENTINEL%02dx7f3q", i)
		sentinels[k] = marker
		switch k {
		case "DATABASE_URL":
			env[k] = "postgres://switchyard:" + marker + "@postgres:5432/switchyard"
		case "REDIS_CACHE_URL", "REDIS_STATE_URL":
			env[k] = "redis://:" + marker + "@redis:6379/0"
		case "SWITCHYARD_ADMIN_TOKEN":
			env[k] = marker + "-admin-token-0123456789abcdef"
		default:
			env[k] = marker
		}
	}
	sentinelValues := writeValues(t, map[string]any{
		"secrets": map[string]any{"create": true, "existingSecret": "", "env": env},
		// A provider whose key only the user's config references.
		"config": map[string]any{"semantic_cache": map[string]any{"embedding_model": "${EXTRA_PROVIDER_KEY:-}"}},
	})

	for _, base := range exampleValues(t) {
		t.Run(filepath.Base(base), func(t *testing.T) {
			objs := mustTemplate(t, base, sentinelValues)
			var secrets []object
			for _, o := range objs {
				if o.Kind == "Secret" {
					secrets = append(secrets, o)
					continue
				}
				for k, v := range env {
					for _, needle := range []string{sentinels[k], base64.StdEncoding.EncodeToString([]byte(v.(string)))} {
						if strings.Contains(o.Text, needle) {
							t.Errorf("%s %s (%s) contains the value of %s", o.Kind, o.Name, o.Template, k)
						}
					}
				}
			}
			// Every value is delivered, by exactly one Secret that the
			// gateway loads with envFrom.
			var holder object
			for _, s := range secrets {
				if strings.Contains(fmt.Sprint(secretData(t, s)), sentinels["DATABASE_URL"]) {
					holder = s
				}
			}
			if holder.Name == "" {
				t.Fatal("no rendered Secret holds secrets.env")
			}
			data := secretData(t, holder)
			for k, v := range env {
				if data[k] != v {
					t.Errorf("Secret %s has %s = %q, want the value from secrets.env", holder.Name, k, data[k])
				}
			}
			c := gatewayContainer(t, gatewayDeployment(t, objs))
			var refs []string
			for _, e := range list(c, "envFrom") {
				refs = append(refs, str(e, "secretRef", "name"))
			}
			if !slices.Contains(refs, holder.Name) {
				t.Errorf("the gateway's envFrom secret refs are %v, want %s among them", refs, holder.Name)
			}
		})
	}
}

// TestRenderConfigLoads loads the rendered config and catalog exactly as
// the gateway would at /etc/switchyard, with the environment the rendered
// Secret provides, for each example values file and the kind test's
// values. A chart default that the gateway rejects fails here, not on a
// cluster.
func TestRenderConfigLoads(t *testing.T) {
	requireHelm(t)
	cases := map[string][]string{"minimal": {minimalValues(t)}}
	for _, f := range exampleValues(t) {
		cases[filepath.Base(f)] = []string{f}
	}
	cases["kind test"] = []string{"testdata/e2e-values.yaml", e2eSecretValues(t, newE2ESecrets(t)), "testdata/upgrade-values.yaml"}

	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			objs := mustTemplate(t, files...)
			cfg, mount := mountedConfigMap(t, objs, "/etc/switchyard/config")
			if sub := str(mount, "subPath"); sub != "" {
				t.Errorf("the config is mounted with subPath %s, which a ConfigMap update never reaches; mount the directory", sub)
			}
			catalog, _ := mountedConfigMap(t, objs, "/etc/switchyard/catalog")

			dir := t.TempDir()
			write := func(sub string, files map[string]string) {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
					t.Fatal(err)
				}
				for name, content := range files {
					if err := os.WriteFile(filepath.Join(dir, sub, name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			write("config", cfg)
			write("catalog", catalog)

			env := map[string]string{}
			for _, s := range find(objs, "Secret") {
				maps.Copy(env, secretData(t, s))
			}
			opts := config.LoadOptions{LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
			if len(env) == 0 {
				// An existing Secret: nothing to read the values from.
				opts.SecretPlaceholders = true
			}
			if _, err := config.Load(filepath.Join(dir, "config", "switchyard.yaml"), opts); err != nil {
				t.Fatalf("the rendered config does not load: %v", err)
			}
		})
	}
}

// TestRenderDefaultConfig checks the chart's default config against the
// contract: compose's config adapted to Kubernetes.
func TestRenderDefaultConfig(t *testing.T) {
	requireHelm(t)
	objs := mustTemplate(t, minimalValues(t))
	data, _ := mountedConfigMap(t, objs, "/etc/switchyard/config")
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(data["switchyard.yaml"]), &cfg); err != nil {
		t.Fatalf("the config ConfigMap has no valid switchyard.yaml: %v", err)
	}
	var providers []string
	for _, p := range list(cfg, "providers") {
		providers = append(providers, str(p, "id"))
	}
	got := map[string]any{
		"include":                 fmt.Sprint(get(cfg, "include")),
		"server.listen":           str(cfg, "server", "listen"),
		"server.admin_listen":     str(cfg, "server", "admin_listen"),
		"server.admin_token":      str(cfg, "server", "admin_token"),
		"server.shutdown_delay":   str(cfg, "server", "shutdown_delay"),
		"server.shutdown_grace":   str(cfg, "server", "shutdown_grace"),
		"storage.mode":            str(cfg, "storage", "mode"),
		"storage.redis.cache_url": str(cfg, "storage", "redis", "cache_url"),
		"storage.redis.state_url": str(cfg, "storage", "redis", "state_url"),
		"storage.ledger.driver":   str(cfg, "storage", "ledger", "driver"),
		"storage.ledger.dsn":      str(cfg, "storage", "ledger", "dsn"),
		"providers":               providers,
	}
	want := map[string]any{
		"include":                 "[../catalog/*.yaml]",
		"server.listen":           ":8080",
		"server.admin_listen":     ":9090",
		"server.admin_token":      "${SWITCHYARD_ADMIN_TOKEN}",
		"server.shutdown_delay":   "5s",
		"server.shutdown_grace":   "60s",
		"storage.mode":            "redis",
		"storage.redis.cache_url": "${REDIS_CACHE_URL}",
		"storage.redis.state_url": "${REDIS_STATE_URL}",
		"storage.ledger.driver":   "postgres",
		"storage.ledger.dsn":      "${DATABASE_URL}",
		"providers":               []string{"openai", "anthropic", "google", "together"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("default config (-want +got):\n%s", diff)
	}

	catalog, _ := mountedConfigMap(t, objs, "/etc/switchyard/catalog")
	sources, err := filepath.Glob(repoPath("configs/catalog/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range sources {
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := catalog[filepath.Base(src)]; !ok || got != string(want) {
			t.Errorf("the catalog ConfigMap's %s is missing or differs from %s", filepath.Base(src), src)
		}
	}
}

// TestRenderPodHardening checks the pods against Pod Security
// "restricted" and the rest of the pod contract: args, ports, probes on
// the admin listener, the rolling update and the helm test hook.
func TestRenderPodHardening(t *testing.T) {
	requireHelm(t)
	objs := mustTemplate(t, minimalValues(t))
	dep := gatewayDeployment(t, objs)
	pod := get(dep.Body, "spec", "template", "spec")
	c := gatewayContainer(t, dep)

	got := map[string]any{
		"args":           fmt.Sprint(get(c, "args")),
		"ports":          portsOf(c),
		"livenessProbe":  probeOf(c, "livenessProbe"),
		"readinessProbe": probeOf(c, "readinessProbe"),
		// Either health route: the startup probe only has to outlast a
		// slow ledger migration.
		"startupProbe":                 strings.TrimPrefix(strings.TrimPrefix(probeOf(c, "startupProbe"), "/healthz"), "/readyz"),
		"strategy":                     str(dep.Body, "spec", "strategy", "type"),
		"maxUnavailable":               fmt.Sprint(get(dep.Body, "spec", "strategy", "rollingUpdate", "maxUnavailable")),
		"replicas":                     fmt.Sprint(get(dep.Body, "spec", "replicas")),
		"automountServiceAccountToken": fmt.Sprint(get(pod, "automountServiceAccountToken")),
	}
	want := map[string]any{
		"args":                         "[serve --config /etc/switchyard/config/switchyard.yaml]",
		"ports":                        "admin=9090 http=8080",
		"livenessProbe":                "/healthz on 9090",
		"readinessProbe":               "/readyz on 9090",
		"startupProbe":                 " on 9090",
		"strategy":                     "RollingUpdate",
		"maxUnavailable":               "0",
		"replicas":                     "3",
		"automountServiceAccountToken": "false",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("gateway pod (-want +got):\n%s", diff)
	}
	checkRestricted(t, "Deployment "+dep.Name, pod)

	// The test hook probes the Service with the gateway's own image.
	var hooks []object
	for _, o := range find(objs, "Pod") {
		if str(o.Body, "metadata", "annotations", "helm.sh/hook") == "test" {
			hooks = append(hooks, o)
		}
	}
	if len(hooks) != 1 {
		t.Fatalf("rendered %d helm test Pods, want 1", len(hooks))
	}
	hook := get(hooks[0].Body, "spec")
	checkRestricted(t, "test Pod "+hooks[0].Name, hook)
	hc := get(hook, "containers", 0)
	if str(hc, "image") != str(c, "image") {
		t.Errorf("the test Pod runs %s, want the gateway's image %s", str(hc, "image"), str(c, "image"))
	}
	if cmd := fmt.Sprint(get(hc, "command"), get(hc, "args")); !strings.Contains(cmd, "healthcheck") || !strings.Contains(cmd, "/readyz") {
		t.Errorf("the test Pod runs %s, want switchyard healthcheck of /readyz", cmd)
	}
	// Retried until it passes or helm test times out: right after helm
	// install --wait, the Service may not be programmed yet.
	if got := str(hook, "restartPolicy"); got != "OnFailure" {
		t.Errorf("the test Pod's restartPolicy is %q, want OnFailure", got)
	}
}

// portsOf lists a container's ports as "name=port", sorted.
func portsOf(c any) string {
	var ps []string
	for _, p := range list(c, "ports") {
		ps = append(ps, fmt.Sprintf("%s=%v", str(p, "name"), get(p, "containerPort")))
	}
	slices.Sort(ps)
	return strings.Join(ps, " ")
}

// probeOf describes an HTTP probe as "path on port", resolving a named
// port to its number.
func probeOf(c any, name string) string {
	p := get(c, name)
	if p == nil {
		return "none"
	}
	port := fmt.Sprint(get(p, "httpGet", "port"))
	for _, cp := range list(c, "ports") {
		if str(cp, "name") == port {
			port = fmt.Sprint(get(cp, "containerPort"))
		}
	}
	return str(p, "httpGet", "path") + " on " + port
}

// checkRestricted checks the fields Pod Security "restricted" requires of
// a pod spec and its containers, plus a read-only root filesystem and no
// service-account token.
func checkRestricted(t *testing.T, what string, pod any) {
	t.Helper()
	sc := get(pod, "securityContext")
	for path, want := range map[string]string{
		"runAsNonRoot": "true", "runAsUser": "65532", "runAsGroup": "65532",
	} {
		if got := fmt.Sprint(get(sc, path)); got != want {
			t.Errorf("%s: securityContext.%s = %s, want %s", what, path, got, want)
		}
	}
	if got := str(sc, "seccompProfile", "type"); got != "RuntimeDefault" {
		t.Errorf("%s: seccompProfile.type = %q, want RuntimeDefault", what, got)
	}
	if got := fmt.Sprint(get(pod, "automountServiceAccountToken")); got != "false" {
		t.Errorf("%s: automountServiceAccountToken = %s, want false", what, got)
	}
	containers := append(list(pod, "containers"), list(pod, "initContainers")...)
	for _, c := range containers {
		csc := get(c, "securityContext")
		name := str(c, "name")
		if got := fmt.Sprint(get(csc, "allowPrivilegeEscalation")); got != "false" {
			t.Errorf("%s: container %s allowPrivilegeEscalation = %s, want false", what, name, got)
		}
		if got := fmt.Sprint(get(csc, "readOnlyRootFilesystem")); got != "true" {
			t.Errorf("%s: container %s readOnlyRootFilesystem = %s, want true", what, name, got)
		}
		if got := fmt.Sprint(get(csc, "capabilities", "drop")); got != "[ALL]" {
			t.Errorf("%s: container %s capabilities.drop = %s, want [ALL]", what, name, got)
		}
		if add := list(csc, "capabilities", "add"); len(add) > 0 {
			t.Errorf("%s: container %s adds capabilities %v", what, name, add)
		}
		if get(csc, "privileged") == true {
			t.Errorf("%s: container %s is privileged", what, name)
		}
	}
}

// TestRenderImage checks the image reference: the chart's appVersion tag
// by default, a digest when one is given.
func TestRenderImage(t *testing.T) {
	requireHelm(t)
	digest := "sha256:" + strings.Repeat("ab", 32)
	cases := []struct {
		name  string
		image map[string]any
		want  string
	}{
		{"default", nil, "ghcr.io/itszarmik/switchyard:v" + appVersion(t)},
		{"tag", map[string]any{"tag": "v9.9.9"}, "ghcr.io/itszarmik/switchyard:v9.9.9"},
		// Kubernetes pulls by the digest whether or not a tag precedes it.
		{"digest", map[string]any{"repository": "registry.example/sy", "digest": digest}, "registry.example/sy:v" + appVersion(t) + "@" + digest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{minimalValues(t)}
			if tc.image != nil {
				files = append(files, writeValues(t, map[string]any{"image": tc.image}))
			}
			c := gatewayContainer(t, gatewayDeployment(t, mustTemplate(t, files...)))
			if got := str(c, "image"); got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

// appVersion reads the chart's appVersion.
func appVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(b, &chart); err != nil {
		t.Fatal(err)
	}
	return chart.AppVersion
}

// unsetKey, as a value in a test case, stands for null, which removes a
// key from the chart's default config.
type unsetKey struct{}

var unset unsetKey

// TestRenderTerminationGrace checks terminationGracePeriodSeconds: derived
// from the rendered config as shutdown_delay + shutdown_grace + 15 s, or
// the value given; a duration the chart cannot parse fails with a message
// naming the setting to use instead.
func TestRenderTerminationGrace(t *testing.T) {
	requireHelm(t)
	cases := []struct {
		name         string
		delay, grace any // nil keeps the chart's default; unset removes it
		explicit     any
		skipSchema   bool   // render with --skip-schema-validation
		want         string // seconds, or the error's text
		wantErr      bool
	}{
		{name: "defaults", want: "80"},
		{name: "seconds", delay: "0s", grace: "30s", want: "45"},
		{name: "minutes", delay: "1m", grace: "2m", want: "195"},
		{name: "minutes and seconds", delay: "10s", grace: "1m30s", want: "115"},
		{name: "hours", delay: "5s", grace: "1h", want: "3620"},
		{name: "grace 90s", grace: "90s", want: "110"},
		// The gateway treats a zero or unset grace as its default, 60s, and
		// an unset delay as 0s (internal/config/defaults.go).
		{name: "zero grace", grace: "0s", want: "80"},
		{name: "zero grace in minutes", grace: "0m", want: "80"},
		{name: "unset grace", grace: unset, want: "80"},
		{name: "unset delay", delay: unset, want: "75"},
		{name: "unsupported", delay: "500ms", want: "terminationGracePeriodSeconds", wantErr: true},
		// Bare numbers, which the gateway rejects (crash-looping every pod):
		// the schema refuses them, and so would the derivation.
		{name: "bare zero", delay: 0, want: "shutdown_delay", wantErr: true},
		{name: "bare zero string", grace: "0", want: "shutdown_grace", wantErr: true},
		{name: "bare zero past the schema", grace: "0", skipSchema: true, want: "terminationGracePeriodSeconds", wantErr: true},
		{name: "explicit overrides", delay: "500ms", explicit: 30, want: "30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := map[string]any{}
			for key, v := range map[string]any{"shutdown_delay": tc.delay, "shutdown_grace": tc.grace} {
				switch v {
				case nil:
				case unset:
					server[key] = nil // null removes the chart's default
				default:
					server[key] = v
				}
			}
			v := map[string]any{"config": map[string]any{"server": server}}
			if tc.explicit != nil {
				v["terminationGracePeriodSeconds"] = tc.explicit
			}
			var extra []string
			if tc.skipSchema {
				extra = []string{"--skip-schema-validation"}
			}
			objs, err := helmTemplate(extra, minimalValues(t), writeValues(t, v))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("render error = %v, want one mentioning %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprint(get(gatewayDeployment(t, objs).Body, "spec", "template", "spec", "terminationGracePeriodSeconds"))
			if got != tc.want {
				t.Errorf("terminationGracePeriodSeconds = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRenderChecksumRollsPods checks the pod template's checksum
// annotations: a change to the config, the catalog or the chart's Secret
// changes them, so the pods roll; other changes leave them alone.
func TestRenderChecksumRollsPods(t *testing.T) {
	requireHelm(t)
	checksums := func(t *testing.T, extra map[string]any) map[string]string {
		t.Helper()
		files := []string{minimalValues(t)}
		if extra != nil {
			files = append(files, writeValues(t, extra))
		}
		dep := gatewayDeployment(t, mustTemplate(t, files...))
		out := map[string]string{}
		ann, _ := get(dep.Body, "spec", "template", "metadata", "annotations").(map[string]any)
		for k, v := range ann {
			if strings.HasPrefix(k, "checksum/") {
				out[k] = fmt.Sprint(v)
			}
		}
		return out
	}
	base := checksums(t, nil)
	if len(base) == 0 {
		t.Fatal("the pod template has no checksum/* annotations")
	}
	cases := []struct {
		name   string
		values map[string]any
		rolls  bool
	}{
		{"config", map[string]any{"config": map[string]any{"logging": map[string]any{"level": "debug"}}}, true},
		{"catalog", map[string]any{"catalog": map[string]any{"extraFiles": map[string]any{"extra.yaml": "aliases:\n  e: claude-haiku-5-5\n"}}}, true},
		{"secret", map[string]any{"secrets": map[string]any{"env": map[string]any{"OPENAI_API_KEY": "sk-render-test"}}}, true},
		{"replicas", map[string]any{"replicaCount": 5}, false},
		{"pod annotation", map[string]any{"podAnnotations": map[string]any{"example.com/team": "llm"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checksums(t, tc.values)
			if changed := !maps.Equal(base, got); changed != tc.rolls {
				t.Errorf("checksums changed = %v, want %v (base %v, got %v)", changed, tc.rolls, base, got)
			}
		})
	}
}

// TestRenderRejectsInvalidValues checks that the chart refuses values that
// cannot work, with a message naming what is wrong (PLAN P9 acceptance 2).
func TestRenderRejectsInvalidValues(t *testing.T) {
	requireHelm(t)
	without := func(key string) map[string]any {
		return map[string]any{"secrets": map[string]any{"env": map[string]any{key: nil}}}
	}
	cases := []struct {
		name   string
		values map[string]any
		want   string // in the error
	}{
		{"no admin token", without("SWITCHYARD_ADMIN_TOKEN"), "SWITCHYARD_ADMIN_TOKEN"},
		{"no database URL", without("DATABASE_URL"), "DATABASE_URL"},
		{"no cache Redis", without("REDIS_CACHE_URL"), "REDIS_CACHE_URL"},
		{"no state Redis", without("REDIS_STATE_URL"), "REDIS_STATE_URL"},
		{"no key hash", without("SWITCHYARD_API_KEY_HASH"), "SWITCHYARD_API_KEY_HASH"},
		{"short admin token", map[string]any{"secrets": map[string]any{"env": map[string]any{"SWITCHYARD_ADMIN_TOKEN": "too-short"}}}, "SWITCHYARD_ADMIN_TOKEN"},
		{"no Secret", map[string]any{"secrets": map[string]any{"create": false}}, "existingSecret"},
		{"no replicas", map[string]any{"replicaCount": 0}, "replicaCount"},
		{"malformed duration", map[string]any{"config": map[string]any{"server": map[string]any{"shutdown_grace": "a minute"}}}, "shutdown_grace"},
		{"bare zero duration", map[string]any{"config": map[string]any{"server": map[string]any{"shutdown_delay": 0}}}, "shutdown_delay"},
		// Kubernetes refuses any other successThreshold for these two.
		{"liveness successThreshold", map[string]any{"probes": map[string]any{"liveness": map[string]any{"successThreshold": 2}}}, "liveness"},
		{"startup successThreshold", map[string]any{"probes": map[string]any{"startup": map[string]any{"successThreshold": 3}}}, "startup"},
		// A PDB that allows no disruption blocks every node drain.
		{"PDB of 0", map[string]any{"podDisruptionBudget": map[string]any{"maxUnavailable": 0}}, "maxUnavailable"},
		{"PDB of 0%", map[string]any{"podDisruptionBudget": map[string]any{"maxUnavailable": "0%"}}, "maxUnavailable"},
		{"PDB of 00%", map[string]any{"podDisruptionBudget": map[string]any{"maxUnavailable": "00%"}}, "maxUnavailable"},
		// $$ escapes a $: $$${VAR} is a $ and then a reference.
		{"reference after an escaped $", map[string]any{"config": map[string]any{"semantic_cache": map[string]any{"embedding_model": "$$${UNSET_AFTER_ESCAPE}"}}}, "UNSET_AFTER_ESCAPE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := helmTemplate(nil, minimalValues(t), writeValues(t, tc.values))
			if err == nil {
				t.Fatal("rendered, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %s:\n%v", tc.want, err)
			}
		})
	}
}

// TestRenderAcceptsValues checks values near the edges of what the chart
// refuses, which must still render.
func TestRenderAcceptsValues(t *testing.T) {
	requireHelm(t)
	cases := map[string]map[string]any{
		"readiness successThreshold": {"probes": map[string]any{"readiness": map[string]any{"successThreshold": 2}}},
		"liveness successThreshold":  {"probes": map[string]any{"liveness": map[string]any{"successThreshold": 1}}},
		"PDB of 10%":                 {"podDisruptionBudget": map[string]any{"maxUnavailable": "10%"}},
		// $$ escapes a $, so $${VAR} is literal text, not a reference.
		"escaped reference": {"config": map[string]any{"semantic_cache": map[string]any{"embedding_model": "$${NOT_A_VARIABLE}"}}},
		// Each secret field as one reference of every kind.
		"secret references": {"config": map[string]any{
			"server":  map[string]any{"admin_token": "${file:/etc/switchyard/secrets/admin-token}"},
			"storage": map[string]any{"ledger": map[string]any{"dsn": "${DATABASE_URL:-postgres://postgres/switchyard}"}},
			"telemetry": map[string]any{"tracing": map[string]any{
				"enabled": true, "endpoint": "http://otel-collector.observability.svc:4318",
			}},
		}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := helmTemplate(nil, minimalValues(t), writeValues(t, v)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRenderErrorsHideSecrets checks the chart's refusals of a bad secret:
// an admin token the gateway would reject, and a literal secret in the
// config, which would land in a ConfigMap. Each fails with a message
// naming the setting, never its value, with the schema checked or not.
func TestRenderErrorsHideSecrets(t *testing.T) {
	requireHelm(t)
	const sentinel = "SENTINELx7f3q"
	token := func(v string) map[string]any {
		return map[string]any{"secrets": map[string]any{"env": map[string]any{"SWITCHYARD_ADMIN_TOKEN": v}}}
	}
	// inConfig returns {config: {k1: {k2: ... v}}}.
	inConfig := func(v any, keys ...string) map[string]any {
		for i := len(keys) - 1; i >= 0; i-- {
			v = map[string]any{keys[i]: v}
		}
		return map[string]any{"config": v}
	}
	lit := "x-" + sentinel + "-0123456789abcdefghijklmnopqrstuvwxyz"
	cases := []struct {
		name   string
		values map[string]any
		want   string // in the error
	}{
		// A newline is what --set-file or a YAML | block adds.
		{"token with a trailing newline", token(sentinel + "-admin-token-0123456789abcdef\n"), "SWITCHYARD_ADMIN_TOKEN"},
		{"token with a space", token(sentinel + "-admin-token 0123456789abcdef"), "SWITCHYARD_ADMIN_TOKEN"},
		{"short token", token(sentinel), "SWITCHYARD_ADMIN_TOKEN"},
		{"literal admin token", inConfig(lit, "server", "admin_token"), "config.server.admin_token"},
		{"literal provider key", inConfig([]any{
			map[string]any{"id": "openai", "type": "openai", "api_key": "${OPENAI_API_KEY:-}"},
			map[string]any{"id": "other", "type": "openai", "api_key": lit},
		}, "providers"), "config.providers[1].api_key"},
		{"literal ledger DSN", inConfig(lit, "storage", "ledger", "dsn"), "config.storage.ledger.dsn"},
		{"literal cache URL", inConfig(lit, "storage", "redis", "cache_url"), "config.storage.redis.cache_url"},
		{"literal state URL", inConfig(lit, "storage", "redis", "state_url"), "config.storage.redis.state_url"},
		// The whole value must be one reference, not a URL around one.
		{"reference inside a URL", inConfig("redis://:"+sentinel+"@redis-state:6379/${REDIS_DB}", "storage", "redis", "state_url"), "config.storage.redis.state_url"},
		{"password in the tracing endpoint", inConfig(map[string]any{
			"enabled": true, "endpoint": "https://user:" + sentinel + "@otel.example:4318",
		}, "telemetry", "tracing"), "config.telemetry.tracing.endpoint"},
	}
	for _, tc := range cases {
		for _, extra := range [][]string{nil, {"--skip-schema-validation"}} {
			t.Run(fmt.Sprint(tc.name, extra), func(t *testing.T) {
				_, err := helmTemplate(extra, minimalValues(t), writeValues(t, tc.values))
				if err == nil {
					t.Fatal("rendered, want an error")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error does not mention %s:\n%v", tc.want, err)
				}
				if strings.Contains(err.Error(), sentinel) {
					t.Errorf("error contains the secret value:\n%v", err)
				}
			})
		}
	}
}

// TestRenderNetworkPolicy checks the NetworkPolicy the chart renders by
// default: ingress to the gateway pods on the API port from anywhere and
// on the admin port from the release's namespace only, egress open; and
// the peers given in its values.
func TestRenderNetworkPolicy(t *testing.T) {
	requireHelm(t)
	policy := func(t *testing.T, v map[string]any) (object, bool) {
		t.Helper()
		files := []string{minimalValues(t)}
		if v != nil {
			files = append(files, writeValues(t, v))
		}
		found := find(mustTemplate(t, files...), "NetworkPolicy")
		if len(found) > 1 {
			t.Fatalf("rendered %d NetworkPolicies, want at most 1", len(found))
		}
		if len(found) == 0 {
			return object{}, false
		}
		return found[0], true
	}
	// ingress lists each rule as "port <- peers", the peers in JSON-ish
	// form; "anyone" when a rule has no from.
	ingress := func(np object) []string {
		var rules []string
		for _, r := range list(np.Body, "spec", "ingress") {
			var ports []string
			for _, p := range list(r, "ports") {
				ports = append(ports, fmt.Sprint(get(p, "port")))
			}
			from := "anyone"
			if peers := list(r, "from"); peers != nil {
				from = fmt.Sprint(peers)
			}
			rules = append(rules, strings.Join(ports, ",")+" <- "+from)
		}
		return rules
	}

	np, ok := policy(t, nil)
	if !ok {
		t.Fatal("no NetworkPolicy by default, want one: without it port 9090 is reachable from every namespace")
	}
	want := []string{"http <- anyone", "admin <- [map[podSelector:map[]]]"}
	if diff := cmp.Diff(want, ingress(np)); diff != "" {
		t.Errorf("default ingress rules (-want +got):\n%s", diff)
	}
	if got := fmt.Sprint(get(np.Body, "spec", "policyTypes")); got != "[Ingress]" {
		t.Errorf("default policyTypes = %s, want [Ingress] (egress unrestricted)", got)
	}
	dep := gatewayDeployment(t, mustTemplate(t, minimalValues(t)))
	podLabels, _ := get(dep.Body, "spec", "template", "metadata", "labels").(map[string]any)
	for k, v := range get(np.Body, "spec", "podSelector", "matchLabels").(map[string]any) {
		if podLabels[k] != v {
			t.Errorf("the policy selects %s=%v, which the gateway pods do not have", k, v)
		}
	}

	monitoring := map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "monitoring"}}}
	np, _ = policy(t, map[string]any{"networkPolicy": map[string]any{"metricsFrom": []any{monitoring}, "ingressFrom": []any{monitoring}}})
	for i, r := range ingress(np) {
		if !strings.Contains(r, "monitoring") {
			t.Errorf("rule %d is %s, want it to allow the given peer", i, r)
		}
	}
	if r := ingress(np)[0]; !strings.Contains(r, "app.kubernetes.io/component:test") {
		t.Errorf("the API rule is %s, want the helm test pod allowed too", r)
	}

	if _, ok := policy(t, map[string]any{"networkPolicy": map[string]any{"enabled": false}}); ok {
		t.Error("networkPolicy.enabled=false still renders a NetworkPolicy")
	}
}

// TestRenderAutoscalingReplicas checks the Deployment's replicas: always
// rendered, so turning autoscaling on never lets them default to 1. With
// autoscaling, helm template (no cluster to look up the live count from)
// renders minReplicas; the kind test checks the live count.
func TestRenderAutoscalingReplicas(t *testing.T) {
	requireHelm(t)
	cases := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{"replicaCount", map[string]any{"replicaCount": 4}, "4"},
		{"autoscaling", map[string]any{"replicaCount": 4, "autoscaling": map[string]any{"enabled": true, "minReplicas": 5, "maxReplicas": 9}}, "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := gatewayDeployment(t, mustTemplate(t, minimalValues(t), writeValues(t, tc.values)))
			if got := fmt.Sprint(get(dep.Body, "spec", "replicas")); got != tc.want {
				t.Errorf("replicas = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRenderNotes checks what helm install prints: the image actually
// installed, and a warning when port 9090 is open to every namespace or
// shut to a Prometheus elsewhere.
func TestRenderNotes(t *testing.T) {
	requireHelm(t)
	const openWarning, scrapeWarning = "reachable from every namespace", "A Prometheus in another"
	monitor := map[string]any{"serviceMonitor": map[string]any{"enabled": true}}
	cases := []struct {
		name          string
		values        map[string]any
		want, notWant []string
	}{
		{"default", nil, []string{"ghcr.io/itszarmik/switchyard:v" + appVersion(t), "this namespace only"}, []string{openWarning, scrapeWarning}},
		{"image tag", map[string]any{"image": map[string]any{"tag": "v9.9.9"}}, []string{"ghcr.io/itszarmik/switchyard:v9.9.9"}, nil},
		{"no network policy", map[string]any{"networkPolicy": map[string]any{"enabled": false}}, []string{openWarning}, []string{scrapeWarning}},
		{"service monitor", map[string]any{"metrics": monitor}, []string{scrapeWarning}, []string{openWarning}},
		{"service monitor allowed", map[string]any{
			"metrics":       monitor,
			"networkPolicy": map[string]any{"metricsFrom": []any{map[string]any{"namespaceSelector": map[string]any{}}}},
		}, []string{"networkPolicy.metricsFrom"}, []string{openWarning, scrapeWarning}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{minimalValues(t)}
			if tc.values != nil {
				files = append(files, writeValues(t, tc.values))
			}
			ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
			defer cancel()
			// --dry-run=client renders the notes without a cluster.
			args := append([]string{"install", "sy", chartDir, "--namespace", "switchyard", "--dry-run=client"}, valuesArgs(files...)...)
			out, err := run(ctx, nil, "helm", args...)
			if err != nil {
				t.Fatal(err)
			}
			_, notes, ok := strings.Cut(out, "\nNOTES:\n")
			if !ok {
				t.Fatalf("helm install --dry-run printed no NOTES:\n%s", out)
			}
			notes = strings.Join(strings.Fields(notes), " ") // the warnings wrap
			for _, w := range tc.want {
				if !strings.Contains(notes, w) {
					t.Errorf("the notes do not mention %q:\n%s", w, notes)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(notes, w) {
					t.Errorf("the notes mention %q:\n%s", w, notes)
				}
			}
		})
	}
}

// TestBundledCopiesMatchSources checks the chart's copies of the catalog
// and the alert rules against their sources, so neither can drift. It
// needs no helm.
func TestBundledCopiesMatchSources(t *testing.T) {
	sources, err := filepath.Glob(repoPath("configs/catalog/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	pairs := map[string]string{repoPath("deploy/prometheus/alerts.yml"): filepath.Join(chartDir, "files", "alerts.yml")}
	for _, src := range sources {
		pairs[src] = filepath.Join(chartDir, "files", "catalog", filepath.Base(src))
	}
	for src, copied := range pairs {
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(copied)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is missing or differs from its source %s; copy it: cp %s %s", copied, src, src, copied)
		}
	}
	copies, err := filepath.Glob(filepath.Join(chartDir, "files", "catalog", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		if _, ok := pairs[repoPath("configs/catalog/"+filepath.Base(c))]; !ok {
			t.Errorf("%s has no source in configs/catalog; delete it", c)
		}
	}
}
