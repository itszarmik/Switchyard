package helm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// chartDir is the chart, relative to this package's directory, where go
// test runs.
const chartDir = "../../deploy/helm/switchyard"

// commandTimeout bounds a helm or kubectl call that is not a wait.
const commandTimeout = 2 * time.Minute

// requireHelm skips the test when helm is not on PATH.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed; the render tests need it (make helm-lint)")
	}
}

// run runs name with args and returns its standard output. A failure's
// error includes the standard error, which is where helm and kubectl
// explain themselves.
func run(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// writeValues writes v as a values file in a temporary directory and
// returns its path. The file is readable only by its owner, since some
// values are (test) secrets.
func writeValues(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// valuesArgs turns values files into -f flags.
func valuesArgs(files ...string) []string {
	var args []string
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return args
}

// object is one rendered Kubernetes object.
type object struct {
	Kind     string
	Name     string
	Text     string         // the YAML document as helm printed it
	Body     map[string]any // the document, decoded
	Template string         // the chart template it came from ("# Source:")
}

// helmTemplate renders the chart with the values files as release "sy"
// in namespace switchyard. It returns helm's error, standard error
// included, when rendering fails.
func helmTemplate(extra []string, files ...string) ([]object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	args := append([]string{"template", "sy", chartDir, "--namespace", "switchyard"}, valuesArgs(files...)...)
	out, err := run(ctx, nil, "helm", append(args, extra...)...)
	if err != nil {
		return nil, err
	}
	return parseObjects(out)
}

// mustTemplate is helmTemplate for renders that must succeed.
func mustTemplate(t *testing.T, files ...string) []object {
	t.Helper()
	objs, err := helmTemplate(nil, files...)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// parseObjects splits a multi-document YAML stream into objects, skipping
// empty documents.
func parseObjects(stream string) ([]object, error) {
	var objs []object
	for doc := range strings.SplitSeq(stream, "\n---") {
		doc = strings.TrimPrefix(doc, "\n")
		var body map[string]any
		if err := yaml.Unmarshal([]byte(doc), &body); err != nil {
			return nil, fmt.Errorf("parse rendered document: %w\n%s", err, doc)
		}
		if body == nil {
			continue
		}
		o := object{Text: doc, Body: body}
		o.Kind, _ = body["kind"].(string)
		o.Name = str(body, "metadata", "name")
		for line := range strings.SplitSeq(doc, "\n") {
			if src, ok := strings.CutPrefix(line, "# Source: "); ok {
				o.Template = src
				break
			}
		}
		objs = append(objs, o)
	}
	if len(objs) == 0 {
		return nil, errors.New("helm rendered no objects")
	}
	return objs, nil
}

// get walks m along path (map keys, or list indexes as ints) and returns
// what it finds, or nil.
func get(m any, path ...any) any {
	cur := m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = mm[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			cur = l[k]
		}
	}
	return cur
}

// str is get for a string; it returns "" for anything else.
func str(m any, path ...any) string {
	s, _ := get(m, path...).(string)
	return s
}

// list is get for a list.
func list(m any, path ...any) []any {
	l, _ := get(m, path...).([]any)
	return l
}

// find returns the objects of kind.
func find(objs []object, kind string) []object {
	var out []object
	for _, o := range objs {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// one returns the only object of kind, failing the test otherwise.
func one(t *testing.T, objs []object, kind string) object {
	t.Helper()
	found := find(objs, kind)
	if len(found) != 1 {
		t.Fatalf("rendered %d objects of kind %s, want 1", len(found), kind)
	}
	return found[0]
}

// gatewayDeployment is the chart's Deployment.
func gatewayDeployment(t *testing.T, objs []object) object {
	t.Helper()
	return one(t, objs, "Deployment")
}

// gatewayContainer is the Deployment's switchyard container: the one that
// runs `serve`.
func gatewayContainer(t *testing.T, dep object) map[string]any {
	t.Helper()
	for _, c := range list(dep.Body, "spec", "template", "spec", "containers") {
		if strings.Contains(fmt.Sprint(get(c, "args")), "serve") {
			return c.(map[string]any)
		}
	}
	t.Fatalf("Deployment %s has no container with args serve", dep.Name)
	return nil
}

// secretData returns the data of a Secret, decoded, with stringData
// merged in.
func secretData(t *testing.T, s object) map[string]string {
	t.Helper()
	out := map[string]string{}
	if m, ok := get(s.Body, "data").(map[string]any); ok {
		for k, v := range m {
			b, err := base64.StdEncoding.DecodeString(fmt.Sprint(v))
			if err != nil {
				t.Fatalf("Secret %s data %s is not base64: %v", s.Name, k, err)
			}
			out[k] = string(b)
		}
	}
	if m, ok := get(s.Body, "stringData").(map[string]any); ok {
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// mountedConfigMap returns the data of the ConfigMap the gateway container
// mounts at dir, and the volume mount itself.
func mountedConfigMap(t *testing.T, objs []object, dir string) (data map[string]string, mount map[string]any) {
	t.Helper()
	dep := gatewayDeployment(t, objs)
	var volume string
	for _, m := range list(gatewayContainer(t, dep), "volumeMounts") {
		if str(m, "mountPath") == dir {
			volume, mount = str(m, "name"), m.(map[string]any)
		}
	}
	if volume == "" {
		t.Fatalf("the gateway container mounts nothing at %s", dir)
	}
	var cm string
	for _, v := range list(dep.Body, "spec", "template", "spec", "volumes") {
		if str(v, "name") == volume {
			cm = str(v, "configMap", "name")
		}
	}
	if cm == "" {
		t.Fatalf("volume %s, mounted at %s, is not a ConfigMap", volume, dir)
	}
	for _, o := range find(objs, "ConfigMap") {
		if o.Name == cm {
			data = map[string]string{}
			for k, v := range get(o.Body, "data").(map[string]any) {
				data[k] = fmt.Sprint(v)
			}
			return data, mount
		}
	}
	t.Fatalf("ConfigMap %s, mounted at %s, is not rendered", cm, dir)
	return nil, nil
}

// repoPath is a path relative to the repository root.
func repoPath(rel string) string {
	return filepath.Join("..", "..", filepath.FromSlash(rel))
}
