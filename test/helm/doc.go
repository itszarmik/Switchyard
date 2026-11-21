// Package helm tests the Helm chart in deploy/helm/switchyard.
//
// The render tests (render_test.go) run `helm template` and check what
// the chart produces: secrets only in Secrets, the rendered config valid
// as the gateway loads it, the pod hardening, the derived
// terminationGracePeriodSeconds, the checksum that rolls the pods and the
// values the chart must reject. They skip when helm is not on PATH. The
// bundled copies of the catalog and the alert rules are checked against
// their sources without helm. `make helm-lint` runs them, with helm lint
// and kubeconform.
//
// The kind test (kind_test.go) installs the chart on a kind cluster, with
// in-cluster Redis, Postgres and a fake provider (testdata/deps.yaml),
// and checks it end to end: Pod Security "restricted", helm test right
// after helm install --wait, traffic through a NodePort spread over the
// replicas, the Postgres ledger, a rate limit shared across replicas, the
// default NetworkPolicy, a rolling upgrade under load with streams in
// flight, a failed upgrade that Helm rolls back, and turning autoscaling
// on without losing replicas. It is opt-in: it skips unless
// SWITCHYARD_HELM_E2E=1, and expects the cluster `make helm-test` sets up:
//
//	make helm-test                       create the cluster, build and load the images, run, delete
//	make helm-test KIND_KEEP=1           keep the cluster afterwards
//	make helm-test KIND_REUSE=1          use the existing cluster KIND_CLUSTER, and keep it
//
// It reads HELM_E2E_CONTEXT (the kubectl context, default
// kind-switchyard-helm), HELM_E2E_URL (the gateway through the NodePort,
// default http://127.0.0.1:28080, the host port in testdata/kind.yaml)
// and HELM_E2E_IMAGE (the loaded gateway image, default switchyard:dev;
// the fake's is switchyard-fakeupstream:dev, as deps.yaml says), and
// passes every kubectl and helm call the context explicitly.
package helm
