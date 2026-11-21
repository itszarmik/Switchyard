package helm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
)

// The kind test's fixed names: testdata/namespaces.yaml and the service
// URLs in testdata/e2e-values.yaml use them.
const (
	chartNamespace = "switchyard-e2e"
	depsNamespace  = "switchyard-e2e-deps"
	release        = "sy"
	depsHost       = "svc.cluster.local"
	// postgresPassword is deps.yaml's throwaway password.
	postgresPassword = "switchyard-e2e-not-a-secret"
)

// Rate limit of the e2e-limited key in e2e-values.yaml: 1 a minute with a
// burst of 9, so 10 requests are allowed at once and then one a minute.
const (
	limitedBurst = 9
	limitedRPM   = 1
)

// e2eSecrets are one run's secrets, all random.
type e2eSecrets struct {
	adminToken              string
	apiKey, apiKeyHash      string // the e2e key
	limitedKey, limitedHash string // the rate-limited e2e-limited key
	fakeKey                 string // the fake provider's
}

func newE2ESecrets(t *testing.T) e2eSecrets {
	t.Helper()
	var s e2eSecrets
	var err error
	if s.apiKey, s.apiKeyHash, err = harness.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	if s.limitedKey, s.limitedHash, err = harness.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	s.adminToken, s.fakeKey = randomToken(t), randomToken(t)
	return s
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// e2eSecretValues writes the values file with secrets.env for s.
func e2eSecretValues(t *testing.T, s e2eSecrets) string {
	t.Helper()
	deps := func(svc string) string { return svc + "." + depsNamespace + "." + depsHost }
	return writeValues(t, map[string]any{"secrets": map[string]any{"env": map[string]any{
		"SWITCHYARD_ADMIN_TOKEN":  s.adminToken,
		"DATABASE_URL":            "postgres://switchyard:" + postgresPassword + "@" + deps("postgres") + ":5432/switchyard?sslmode=disable",
		"REDIS_CACHE_URL":         "redis://" + deps("redis-cache") + ":6379/0",
		"REDIS_STATE_URL":         "redis://" + deps("redis-state") + ":6379/0",
		"SWITCHYARD_API_KEY_HASH": s.apiKeyHash,
		"E2E_LIMITED_KEY_HASH":    s.limitedHash,
		"FAKE_API_KEY":            s.fakeKey,
	}}})
}

// kindEnv is the cluster the test runs against.
type kindEnv struct {
	t          *testing.T
	kubeCtx    string
	url        string // the gateway through the NodePort
	image      string // repository:tag of the gateway image
	selector   string // the gateway pods' label selector
	container  string // the gateway container's name
	client     *http.Client
	valueFiles []string
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func (e *kindEnv) kubectl(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--context", e.kubeCtx}, args...)...)
}

func (e *kindEnv) mustKubectl(stdin io.Reader, args ...string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := e.kubectl(ctx, stdin, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// helm runs helm against the cluster, failing the test on an error or on
// a Pod Security warning, which the chart's namespace reports for any
// violation. It returns how long the call took.
func (e *kindEnv) helm(timeout time.Duration, args ...string) time.Duration {
	e.t.Helper()
	took, output, err := e.tryHelm(timeout, args...)
	if err != nil {
		e.t.Fatalf("helm %s: %v\n%s", args[0], err, output)
	}
	return took
}

// tryHelm is helm for a call that may fail: it returns the error and
// helm's output, standard error included, instead of failing the test. A
// Pod Security warning still fails it.
func (e *kindEnv) tryHelm(timeout time.Duration, args ...string) (took time.Duration, output string, err error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "helm", append([]string{"--kube-context", e.kubeCtx}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	took = time.Since(start)
	if strings.Contains(stderr.String(), "PodSecurity") {
		e.t.Errorf("helm %s reported Pod Security violations:\n%s", args[0], stderr.String())
	}
	return took, string(out) + stderr.String(), err
}

// TestKind installs the chart on kind and checks PLAN P9 acceptance 3 to 6.
// See the package documentation for how to run it.
func TestKind(t *testing.T) {
	if os.Getenv("SWITCHYARD_HELM_E2E") != "1" {
		t.Skip("set SWITCHYARD_HELM_E2E=1 to run the kind test; make helm-test sets up the cluster and does")
	}
	requireHelm(t)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatal("kubectl is not installed")
	}
	e := &kindEnv{
		t:       t,
		kubeCtx: envOr("HELM_E2E_CONTEXT", "kind-switchyard-helm"),
		url:     strings.TrimSuffix(envOr("HELM_E2E_URL", "http://127.0.0.1:28080"), "/"),
		image:   envOr("HELM_E2E_IMAGE", "switchyard:dev"),
		// One connection per request, so kube-proxy picks a pod for each
		// and a pod that is shutting down is never reused.
		client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	}
	t.Cleanup(e.dumpOnFailure)
	s := newE2ESecrets(t)
	repo, tag, ok := strings.Cut(e.image, ":")
	if !ok {
		t.Fatalf("HELM_E2E_IMAGE=%q, want repository:tag", e.image)
	}
	e.valueFiles = []string{
		"testdata/e2e-values.yaml",
		e2eSecretValues(t, s),
		writeValues(t, map[string]any{"image": map[string]any{"repository": repo, "tag": tag}}),
	}

	e.setUp(s)
	e.install()
	e.networkPolicy()
	e.chatRecordedInLedger(s)
	e.spreadOverPods(s)
	e.rateLimitShared(s)
	e.upgradeUnderLoad(s)
	e.failedUpgradeRollsBack(s)
	e.autoscalingKeepsReplicas()
}

// currentValues are the values files of the release as upgradeUnderLoad
// leaves it, plus extra; later upgrades start from them.
func (e *kindEnv) currentValues(extra ...string) []string {
	return append(append(slices.Clone(e.valueFiles), "testdata/upgrade-values.yaml"), extra...)
}

// setUp starts from empty namespaces, checks that the chart's enforces
// Pod Security "restricted", and deploys the dependencies.
func (e *kindEnv) setUp(s e2eSecrets) {
	t := e.t
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// A reused cluster may hold an earlier run's namespaces (KIND_REUSE=1).
	if _, err := e.kubectl(ctx, nil, "delete", "namespace", chartNamespace, depsNamespace, "--ignore-not-found", "--wait"); err != nil {
		t.Fatal(err)
	}
	e.mustKubectl(nil, "apply", "-f", "testdata/namespaces.yaml")

	// Acceptance 6: the namespace refuses a pod that is not restricted.
	root := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"root-probe"},"spec":{"containers":[{"name":"c","image":"` + e.image + `"}]}}`
	_, err := e.kubectl(ctx, strings.NewReader(root), "apply", "--dry-run=server", "-n", chartNamespace, "-f", "-")
	if err == nil || !strings.Contains(err.Error(), "violates PodSecurity") {
		t.Fatalf("namespace %s admitted a pod that is not restricted (err = %v); want Pod Security restricted enforced", chartNamespace, err)
	}

	fakeSecret, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "fakeupstream"},
		"stringData": map[string]string{"api-key": s.fakeKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mustKubectl(bytes.NewReader(fakeSecret), "apply", "-n", depsNamespace, "-f", "-")
	e.mustKubectl(nil, "apply", "-n", depsNamespace, "-f", "testdata/deps.yaml")
	if _, err := e.kubectl(ctx, nil, "wait", "-n", depsNamespace, "--for=condition=Available", "deployment", "--all", "--timeout=4m"); err != nil {
		t.Fatalf("the dependencies did not become available: %v", err)
	}
	t.Logf("namespaces and dependencies ready in %s", time.Since(start).Round(time.Second))
}

// install runs helm install and, at once, helm test, as the chart's NOTES
// and docs/kubernetes.md tell users to; then it waits until the gateway
// answers through the NodePort (acceptance 3).
func (e *kindEnv) install() {
	t := e.t
	took := e.helm(6*time.Minute, append([]string{"install", release, chartDir, "-n", chartNamespace,
		"--wait", "--timeout", "5m"}, valuesArgs(e.valueFiles...)...)...)
	t.Logf("helm install --wait: %s", took.Round(time.Second))
	// No wait in between: the test pod retries until kube-proxy has
	// programmed the Service.
	took = e.helm(3*time.Minute, "test", release, "-n", chartNamespace, "--timeout", "2m")
	t.Logf("helm test: passed in %s", took.Round(time.Second))

	var dep struct {
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `json:"name"`
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	out := e.mustKubectl(nil, "get", "deployment", "-n", chartNamespace, "-l", "app.kubernetes.io/instance="+release, "-o", "jsonpath={.items[0]}")
	if err := json.Unmarshal([]byte(out), &dep); err != nil {
		t.Fatalf("decode the Deployment: %v", err)
	}
	if dep.Status.ReadyReplicas != 3 {
		t.Fatalf("the Deployment has %d ready replicas, want 3", dep.Status.ReadyReplicas)
	}
	var sel []string
	for _, k := range slices.Sorted(maps.Keys(dep.Spec.Selector.MatchLabels)) {
		sel = append(sel, k+"="+dep.Spec.Selector.MatchLabels[k])
	}
	e.selector = strings.Join(sel, ",")
	for _, c := range dep.Spec.Template.Spec.Containers {
		if slices.Contains(c.Args, "serve") {
			e.container = c.Name
		}
	}
	if e.container == "" {
		t.Fatal("the Deployment has no container running serve")
	}

	// The host's port mapping to the NodePort can lag behind the Service:
	// wait until it answers.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.url+"/readyz", nil)
		resp, err := e.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the gateway does not answer at %s/readyz through the NodePort: %v", e.url, err)
		case <-time.After(time.Second):
		}
	}
}

// networkPolicy checks the chart's default NetworkPolicy, enforced by
// kindnet: from another namespace the API port answers and the admin port
// (/metrics, /admin/v1/*) does not; from the release's namespace the admin
// port answers, which also shows the refusal is the policy's doing.
func (e *kindEnv) networkPolicy() {
	t := e.t
	ip := strings.TrimSpace(e.mustKubectl(nil, "get", "pods", "-n", chartNamespace, "-l", e.selector,
		"-o", "jsonpath={.items[0].status.podIP}"))
	if ip == "" {
		t.Fatal("the first gateway pod has no IP")
	}
	cases := []struct {
		ns, port string
		reach    bool
	}{
		{depsNamespace, "8080", true},
		{depsNamespace, "9090", false},
		{chartNamespace, "9090", true},
	}
	for _, tc := range cases {
		url := "http://" + ip + ":" + tc.port + "/readyz"
		ok, out := e.probeFrom(tc.ns, "np-probe-"+tc.port, url)
		if ok != tc.reach {
			t.Errorf("from namespace %s, GET %s passed = %v, want %v: %s", tc.ns, url, ok, tc.reach, out)
			continue
		}
		t.Logf("network policy: from namespace %s, port %s reachable = %v (%s)", tc.ns, tc.port, ok, strings.TrimSpace(out))
	}
}

// probeFrom runs `switchyard healthcheck --url url` in a throwaway pod in
// namespace ns, with the gateway image and Pod Security "restricted"
// settings, and returns whether it passed and what it printed.
func (e *kindEnv) probeFrom(ns, name, url string) (passed bool, output string) {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": "probe", "image": e.image, "imagePullPolicy": "Never",
				"args": []string{"healthcheck", "--url", url, "--timeout", "5s"},
				"securityContext": map[string]any{
					"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
					"capabilities": map[string]any{"drop": []string{"ALL"}},
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.kubectl(ctx, bytes.NewReader(manifest), "apply", "-n", ns, "-f", "-"); err != nil {
		t.Fatal(err)
	}
	defer e.mustKubectl(nil, "delete", "pod", "-n", ns, name, "--wait=false")
	for {
		phase, err := e.kubectl(ctx, nil, "get", "pod", "-n", ns, name, "-o", "jsonpath={.status.phase}")
		if err == nil && (phase == "Succeeded" || phase == "Failed") {
			logs, err := e.kubectl(ctx, nil, "logs", "-n", ns, name)
			if err != nil {
				t.Fatal(err)
			}
			return phase == "Succeeded", logs
		}
		select {
		case <-ctx.Done():
			t.Fatalf("probe pod %s/%s did not finish (phase %q, %v)", ns, name, phase, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// result is the outcome of one request through the NodePort.
type result struct {
	status    int
	requestID string
	body      []byte
	err       error
}

func (r result) String() string {
	if r.err != nil {
		return r.err.Error()
	}
	return fmt.Sprintf("%d %s", r.status, bytes.TrimSpace(r.body))
}

// chat sends a non-streaming chat completion with key; userAgent tags it
// in the access logs.
func (e *kindEnv) chat(ctx context.Context, key, model, userAgent string) result {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"Say hello."}]}`
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return result{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := e.client.Do(req)
	if err != nil {
		return result{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, requestID: resp.Header.Get("X-Request-Id"), body: b, err: err}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// chatRecordedInLedger sends one chat request to the fake provider and
// finds its row in the Postgres ledger (acceptance 3).
func (e *kindEnv) chatRecordedInLedger(s e2eSecrets) {
	t := e.t
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := e.chat(ctx, s.apiKey, "fake-chat", "switchyard-helm-e2e/ledger")
	if r.err != nil || r.status != http.StatusOK || !strings.Contains(string(r.body), "quick brown fox") {
		t.Fatalf("chat through the NodePort: %v, want 200 with the fake's answer", r)
	}
	if !uuidRE.MatchString(r.requestID) {
		t.Fatalf("X-Request-Id = %q, want a UUID", r.requestID)
	}
	// The ID is checked above, so it is safe to put in the query.
	query := "SELECT status, key_id, deployment, stream FROM usage_events WHERE id = '" + r.requestID + "'"
	want := "200|e2e|fake-chat@fake|f"
	var got string
	for {
		out, err := e.kubectl(ctx, nil, "exec", "-n", depsNamespace, "deploy/postgres", "--",
			"psql", "-U", "switchyard", "-d", "switchyard", "-tAc", query)
		got = strings.TrimSpace(out)
		if err == nil && got != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("request %s is not in the ledger (last: %q, %v)", r.requestID, got, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if got != want {
		t.Fatalf("ledger row for %s = %q, want %q", r.requestID, got, want)
	}
	t.Logf("ledger: request %s recorded as %s", r.requestID, got)
}

// pod is a gateway pod.
type pod struct {
	name, checksums string
	ready           bool
	terminating     bool
}

func (e *kindEnv) pods(ctx context.Context) ([]pod, error) {
	out, err := e.kubectl(ctx, nil, "get", "pods", "-n", chartNamespace, "-l", e.selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
				Annotations       map[string]string `json:"annotations"`
				OwnerReferences   []struct {
					Kind string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode pods: %w", err)
	}
	var pods []pod
	for _, it := range list.Items {
		// Only the Deployment's pods, not the helm test hook's.
		if len(it.Metadata.OwnerReferences) == 0 || it.Metadata.OwnerReferences[0].Kind != "ReplicaSet" {
			continue
		}
		p := pod{name: it.Metadata.Name, terminating: it.Metadata.DeletionTimestamp != nil}
		var sums []string
		for _, k := range slices.Sorted(maps.Keys(it.Metadata.Annotations)) {
			if strings.HasPrefix(k, "checksum/") {
				sums = append(sums, k+"="+it.Metadata.Annotations[k])
			}
		}
		p.checksums = strings.Join(sums, " ")
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				p.ready = true
			}
		}
		pods = append(pods, p)
	}
	return pods, nil
}

// accessLog counts one pod's access-log lines for one user agent.
type accessLog struct {
	statuses map[int]int // status -> requests
	total    int
}

// accessLogs reads the access logs of the current gateway pods and
// returns, per pod, the requests with userAgent.
func (e *kindEnv) accessLogs(ctx context.Context, userAgent string) (map[string]accessLog, error) {
	pods, err := e.pods(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]accessLog{}
	for _, p := range pods {
		logs, err := e.kubectl(ctx, nil, "logs", "-n", chartNamespace, p.name, "-c", e.container)
		if err != nil {
			return nil, err
		}
		out[p.name] = parseAccessLog(logs, userAgent)
	}
	return out, nil
}

// logLine is the part of a JSON log line the test reads.
type logLine struct {
	Msg       string `json:"msg"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	UserAgent string `json:"user_agent"`
	RequestID string `json:"request_id"`
}

func parseAccessLog(logs, userAgent string) accessLog {
	a := accessLog{statuses: map[int]int{}}
	for line := range strings.SplitSeq(logs, "\n") {
		var l logLine
		if json.Unmarshal([]byte(line), &l) != nil || l.Msg != "request" || l.UserAgent != userAgent {
			continue
		}
		a.statuses[l.Status]++
		a.total++
	}
	return a
}

// awaitAccessLogs polls the access logs until they show want requests
// with userAgent: the gateway writes them asynchronously.
func (e *kindEnv) awaitAccessLogs(userAgent string, want int) map[string]accessLog {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		logs, err := e.accessLogs(ctx, userAgent)
		total := 0
		for _, l := range logs {
			total += l.total
		}
		if err == nil && total >= want {
			return logs
		}
		select {
		case <-ctx.Done():
			e.t.Fatalf("the access logs show %d of %d requests from %s (%v)", total, want, userAgent, err)
		case <-time.After(time.Second):
		}
	}
}

// spreadOverPods checks that requests through the NodePort reach more than
// one replica (acceptance 3).
func (e *kindEnv) spreadOverPods(s e2eSecrets) {
	t := e.t
	const n, ua = 30, "switchyard-helm-e2e/spread"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := range n {
		if r := e.chat(ctx, s.apiKey, "fake-chat", ua); r.err != nil || r.status != http.StatusOK {
			t.Fatalf("request %d: %v, want 200", i, r)
		}
	}
	logs := e.awaitAccessLogs(ua, n)
	served := map[string]int{}
	for name, l := range logs {
		if l.total > 0 {
			served[name] = l.total
		}
	}
	if len(served) < 2 {
		t.Fatalf("%d requests were served by %v, want at least 2 pods", n, served)
	}
	t.Logf("spread: %d requests served by %d pods: %v", n, len(served), served)
}

// rateLimitShared sends a burst from the rate-limited key, spread over the
// replicas, and checks that the key's limit holds across them
// (acceptance 4): exactly as many are allowed as one limiter allows.
func (e *kindEnv) rateLimitShared(s e2eSecrets) {
	t := e.t
	const n, workers, ua = 90, 10, "switchyard-helm-e2e/ratelimit"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results := make([]result, n)
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				results[i] = e.chat(ctx, s.limitedKey, "fake-chat", ua)
			}
		})
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	statuses := map[int]int{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d: %v", i, r.err)
		}
		statuses[r.status]++
	}
	// GCRA: burst+1 at once, then one per interval.
	wantOK := limitedBurst + 1 + int(elapsed/(time.Minute/limitedRPM))
	if statuses[http.StatusOK] != wantOK || statuses[http.StatusTooManyRequests] != n-wantOK {
		t.Fatalf("%d requests in %s: statuses %v, want %d × 200 and %d × 429", n, elapsed.Round(time.Millisecond), statuses, wantOK, n-wantOK)
	}

	// The requests were spread: with a limiter per replica, each pod would
	// have allowed up to burst+1 of its own.
	logs := e.awaitAccessLogs(ua, n)
	perPod, perReplica := map[string]string{}, 0
	for name, l := range logs {
		if l.total == 0 {
			continue
		}
		perPod[name] = fmt.Sprintf("%d (%d allowed)", l.total, l.statuses[http.StatusOK])
		perReplica += min(l.total, limitedBurst+1)
	}
	if perReplica <= wantOK {
		t.Fatalf("the requests were not spread enough to tell a shared limit from per-replica ones: %v", perPod)
	}
	t.Logf("rate limit: %d requests in %s, %d allowed, %d refused with 429; per pod %v; per-replica limits would have allowed %d",
		n, elapsed.Round(time.Millisecond), statuses[http.StatusOK], statuses[http.StatusTooManyRequests], perPod, perReplica)
}

// stream is a streaming request kept in flight across the upgrade.
type stream struct {
	requestID string
	events    int
	done      bool // received data: [DONE]
	err       error
	took      time.Duration
}

// startStream sends a streaming chat completion to the slow fake and
// reads it to the end in the background. first is closed when the first
// event arrives (or the stream fails before it).
func (e *kindEnv) startStream(ctx context.Context, key string, out *stream, first chan<- struct{}, wg *sync.WaitGroup) {
	wg.Go(func() {
		var once sync.Once
		defer once.Do(func() { close(first) })
		start := time.Now()
		defer func() { out.took = time.Since(start) }()
		body := `{"model":"fake-slow","stream":true,"messages":[{"role":"user","content":"Tell a long story."}]}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			out.err = err
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "switchyard-helm-e2e/stream")
		resp, err := e.client.Do(req)
		if err != nil {
			out.err = err
			return
		}
		defer resp.Body.Close()
		out.requestID = resp.Header.Get("X-Request-Id")
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			out.err = fmt.Errorf("status %d: %s", resp.StatusCode, b)
			return
		}
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			if data == "[DONE]" {
				out.done = true
				continue
			}
			out.events++
			once.Do(func() { close(first) })
		}
		out.err = sc.Err()
	})
}

// followLogs follows a pod's log until its container exits or ctx is
// done.
func (e *kindEnv) followLogs(ctx context.Context, name string, buf *bytes.Buffer, wg *sync.WaitGroup) {
	cmd := exec.CommandContext(ctx, "kubectl", "--context", e.kubeCtx, "logs", "-f", "-n", chartNamespace, name, "-c", e.container)
	cmd.Stdout = buf
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	wg.Go(func() { _ = cmd.Wait() })
}

// upgradeUnderLoad runs helm upgrade with a config change while steady
// load runs and streams are in flight (acceptance 5): no request fails,
// every stream completes, and every pod is replaced.
func (e *kindEnv) upgradeUnderLoad(s e2eSecrets) {
	t := e.t
	const streams, loadInterval, loadUA = 3, 100 * time.Millisecond, "switchyard-helm-e2e/upgrade"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	old, err := e.pods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldNames := map[string]string{} // name -> checksums
	for _, p := range old {
		oldNames[p.name] = p.checksums
	}

	// Follow the old pods' logs to their end: their shutdown and the
	// streams' access lines are written after the pods leave the API.
	logs := make(map[string]*bytes.Buffer, len(old))
	var followers sync.WaitGroup
	for _, p := range old {
		logs[p.name] = new(bytes.Buffer)
		e.followLogs(ctx, p.name, logs[p.name], &followers)
	}

	var streamsWG sync.WaitGroup
	ss := make([]stream, streams)
	for i := range ss {
		first := make(chan struct{})
		e.startStream(ctx, s.apiKey, &ss[i], first, &streamsWG)
		<-first
	}

	// Steady load: one request every loadInterval, each on its own
	// connection, whatever the previous ones are doing.
	var (
		loadMu   sync.Mutex
		load     []result
		loadWG   sync.WaitGroup
		stopLoad = make(chan struct{})
	)
	go func() {
		tick := time.NewTicker(loadInterval)
		defer tick.Stop()
		for {
			select {
			case <-stopLoad:
				return
			case <-tick.C:
				loadWG.Go(func() {
					r := e.chat(ctx, s.apiKey, "fake-chat", loadUA)
					loadMu.Lock()
					load = append(load, r)
					loadMu.Unlock()
				})
			}
		}
	}()

	start := time.Now()
	took := e.helm(6*time.Minute, append([]string{"upgrade", release, chartDir, "-n", chartNamespace,
		"--wait", "--timeout", "5m"}, valuesArgs(e.currentValues()...)...)...)

	// helm returns once the new pods are ready; the old ones may still be
	// draining. Keep the load on until they are gone.
	var current []pod
	for {
		current, err = e.pods(ctx)
		gone := err == nil
		for _, p := range current {
			if _, ok := oldNames[p.name]; ok {
				gone = false
			}
		}
		if gone {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("old pods still present after the upgrade: %v (%v)", current, err)
		case <-time.After(time.Second):
		}
	}
	replaced := time.Since(start)
	close(stopLoad)
	loadWG.Wait()
	streamsWG.Wait()
	followers.Wait()

	failed := 0
	for _, r := range load {
		if r.err != nil || r.status != http.StatusOK {
			if failed < 10 {
				t.Errorf("request during the upgrade failed: %v", r)
			}
			failed++
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d requests failed during the upgrade", failed, len(load))
	}

	// Every stream completed, and at least one was in flight on a pod when
	// it got SIGTERM: its pod logged "shutdown started" before the
	// stream's access line, which is written when the stream ends.
	crossed := 0
	for i, st := range ss {
		if st.err != nil || !st.done || st.events < 10 {
			t.Errorf("stream %d (%s): %d events, [DONE] %v, error %v; want it complete", i, st.requestID, st.events, st.done, st.err)
		}
		where := "in no followed pod log"
		for name, buf := range logs {
			switch served, crossedIt := streamServed(buf.String(), st.requestID); {
			case crossedIt:
				crossed++
				where = "in flight on " + name + " when it began shutting down"
			case served:
				where = "served by " + name + ", which finished it before shutting down"
			}
		}
		t.Logf("stream %d (%s): %d events in %s, %s", i, st.requestID, st.events, st.took.Round(time.Second), where)
	}
	if crossed == 0 {
		t.Errorf("none of the %d streams was in flight on its pod at SIGTERM; lengthen the slow fake's streams (deps.yaml)", streams)
	}

	if len(current) != 3 {
		t.Errorf("%d pods after the upgrade, want 3", len(current))
	}
	for _, p := range current {
		if !p.ready || p.terminating {
			t.Errorf("pod %s after the upgrade: ready %v, terminating %v", p.name, p.ready, p.terminating)
		}
		for _, sums := range oldNames {
			if p.checksums == sums {
				t.Errorf("pod %s has the old pods' checksums %q, want new ones", p.name, sums)
			}
		}
	}

	// The new config is live on every new pod: the alias it adds works.
	for i := range 9 {
		if r := e.chat(ctx, s.apiKey, "e2e-upgraded", "switchyard-helm-e2e/upgraded"); r.err != nil || r.status != http.StatusOK {
			t.Errorf("request %d for the upgrade's new alias: %v, want 200", i, r)
		}
	}
	t.Logf("upgrade: helm upgrade --wait %s, all pods replaced in %s; %d requests at %s intervals, %d failed; %d of %d streams crossed a pod's SIGTERM",
		took.Round(time.Second), replaced.Round(time.Second), len(load), loadInterval, failed, crossed, streams)
}

// streamServed reports whether a pod's log has the access line of
// requestID, and whether "shutdown started" comes before it.
func streamServed(logs, requestID string) (served, crossed bool) {
	if requestID == "" {
		return false, false
	}
	shutdown := false
	for line := range strings.SplitSeq(logs, "\n") {
		var l logLine
		if json.Unmarshal([]byte(line), &l) != nil {
			continue
		}
		switch {
		case l.Msg == "shutdown started":
			shutdown = true
		case l.Msg == "request" && l.RequestID == requestID:
			return true, shutdown
		}
	}
	return false, false
}

// podNames returns the names of pods, and whether every one is ready and
// not terminating.
func podNames(pods []pod) (names []string, allReady bool) {
	allReady = true
	for _, p := range pods {
		names = append(names, p.name)
		allReady = allReady && p.ready && !p.terminating
	}
	slices.Sort(names)
	return names, allReady
}

// failedUpgradeRollsBack runs the upgrade docs/kubernetes.md recommends,
// helm upgrade --rollback-on-failure, with a config the gateway refuses:
// the new pod never becomes ready, Helm rolls the release back, and the
// old pods are left serving with the old config back in the ConfigMap, so
// one that restarts later still starts.
func (e *kindEnv) failedUpgradeRollsBack(s e2eSecrets) {
	t := e.t
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	before, err := e.pods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldNames, _ := podNames(before)

	bad := writeValues(t, map[string]any{"config": map[string]any{"logging": map[string]any{"level": "bogus"}}})
	took, output, err := e.tryHelm(3*time.Minute, append([]string{"upgrade", release, chartDir, "-n", chartNamespace,
		"--rollback-on-failure", "--wait", "--timeout", "40s"}, valuesArgs(e.currentValues(bad)...)...)...)
	if err == nil {
		t.Fatalf("helm upgrade with an invalid config succeeded, want it to fail and roll back:\n%s", output)
	}
	_, out, err := e.tryHelm(commandTimeout, "status", release, "-n", chartNamespace, "-o", "json")
	if err != nil {
		t.Fatalf("helm status: %v\n%s", err, out)
	}
	var status struct {
		Info struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("decode helm status: %v\n%s", err, out)
	}
	if status.Info.Status != "deployed" || !strings.Contains(status.Info.Description, "Rollback") {
		t.Fatalf("after the failed upgrade the release is %q (%q), want deployed by a rollback", status.Info.Status, status.Info.Description)
	}

	// The failed upgrade's pod goes; the old pods were never touched.
	var names []string
	for {
		cur, err := e.pods(ctx)
		ready := false
		if err == nil {
			names, ready = podNames(cur)
			if ready && slices.Equal(names, oldNames) {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("after the rollback the gateway pods are %v, want the old ones %v, all ready (%v)", names, oldNames, err)
		case <-time.After(time.Second):
		}
	}
	cfg := e.mustKubectl(nil, "get", "configmap", "-n", chartNamespace, release+"-switchyard-config",
		"-o", `jsonpath={.data.switchyard\.yaml}`)
	if strings.Contains(cfg, "bogus") || !strings.Contains(cfg, "e2e-upgraded") {
		t.Fatalf("after the rollback the config ConfigMap does not hold the previous config:\n%s", cfg)
	}
	for i := range 3 {
		if r := e.chat(ctx, s.apiKey, "e2e-upgraded", "switchyard-helm-e2e/rollback"); r.err != nil || r.status != http.StatusOK {
			t.Fatalf("request %d after the rollback: %v, want 200", i, r)
		}
	}
	t.Logf("failed upgrade: helm upgrade --rollback-on-failure failed and rolled back in %s; %s; the old pods %v serve the old config",
		took.Round(time.Second), status.Info.Description, oldNames)
}

// autoscalingKeepsReplicas turns autoscaling on with helm upgrade. The
// chart keeps rendering the Deployment's live replica count, so the
// Deployment keeps its 3 pods while the HPA takes over: none is stopped.
func (e *kindEnv) autoscalingKeepsReplicas() {
	t := e.t
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before, err := e.pods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldNames, _ := podNames(before)

	on := writeValues(t, map[string]any{"autoscaling": map[string]any{"enabled": true}})
	took := e.helm(3*time.Minute, append([]string{"upgrade", release, chartDir, "-n", chartNamespace,
		"--wait", "--timeout", "2m"}, valuesArgs(e.currentValues(on)...)...)...)

	if got := e.mustKubectl(nil, "get", "deployment", "-n", chartNamespace, "-l", "app.kubernetes.io/instance="+release,
		"-o", "jsonpath={.items[0].spec.replicas}"); got != "3" {
		t.Errorf("with autoscaling on, the Deployment has %s replicas, want 3", got)
	}
	if hpa := e.mustKubectl(nil, "get", "hpa", "-n", chartNamespace, "-o", "name"); strings.TrimSpace(hpa) == "" {
		t.Error("autoscaling.enabled created no HorizontalPodAutoscaler")
	}
	after, err := e.pods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names, ready := podNames(after)
	if !ready || !slices.Equal(names, oldNames) {
		t.Errorf("after turning autoscaling on the gateway pods are %v (all ready %v), want the same %v, all ready", names, ready, oldNames)
	}
	t.Logf("autoscaling: turned on in %s; the Deployment kept its 3 pods", took.Round(time.Second))
}

// dumpOnFailure logs the state of the cluster when the test failed, for
// CI, where the cluster is gone afterwards.
func (e *kindEnv) dumpOnFailure() {
	if !e.t.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, args := range [][]string{
		{"get", "all,events", "-n", chartNamespace},
		{"get", "pods", "-n", depsNamespace},
		{"describe", "pods", "-n", chartNamespace},
		{"logs", "-n", chartNamespace, "-l", "app.kubernetes.io/instance=" + release, "--all-containers", "--tail=50", "--prefix"},
	} {
		out, err := e.kubectl(ctx, nil, args...)
		e.t.Logf("kubectl %s:\n%s%v", strings.Join(args, " "), out, err)
	}
}
