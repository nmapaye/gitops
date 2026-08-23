package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	canaryv1 "github.com/example/canary-operator/pkg/apis/canary/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := canaryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func validCanary(prometheusURL string) *canaryv1.Canary {
	return &canaryv1.Canary{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default", Generation: 1},
		Spec: canaryv1.CanarySpec{
			TargetRef:     "demo",
			StableService: "stable",
			CanaryService: "canary",
			Steps:         []int{10, 50, 100},
			StepInterval:  metav1.Duration{Duration: 30 * time.Second},
			SLO:           canaryv1.SLOConfig{PrometheusURL: prometheusURL, P95LatencyQuery: "p95", ErrorRateQuery: "errors", P95LatencyMsMax: 300, ErrorRateMax: 0.02},
			Abort:         canaryv1.AbortRules{MinErrorBudgetPercent: 10, MaxP95IncreaseMs: 100},
		},
	}
}

func service(name string, weight string) *corev1.Service {
	annotations := map[string]string{}
	if weight != "" {
		annotations["canary.example.io/weight"] = weight
	}
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations}}
}

func metricServer(t *testing.T, p95, errorRate *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := *p95
		if r.URL.Query().Get("query") == "errors" {
			value = *errorRate
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,%q]}]}}`, value)
	}))
}

func TestDefaultsDoNotMutateSpecAndValidationFailsClosed(t *testing.T) {
	cn := validCanary("http://prometheus.example")
	cn.Spec.Steps = nil
	cn.Spec.StepInterval.Duration = 0
	steps := effectiveSteps(cn.Spec.Steps)
	interval := effectiveInterval(cn.Spec.StepInterval.Duration)
	if len(cn.Spec.Steps) != 0 || cn.Spec.StepInterval.Duration != 0 {
		t.Fatal("effective defaults mutated the resource spec")
	}
	if len(steps) != 3 || steps[2] != 100 || interval != 30*time.Second {
		t.Fatalf("unexpected defaults: %v, %s", steps, interval)
	}

	cases := []struct {
		name  string
		steps []int
		edit  func(*canaryv1.Canary)
	}{
		{name: "steps must increase", steps: []int{50, 10, 100}},
		{name: "steps must finish", steps: []int{10, 50}},
		{name: "services differ", steps: []int{100}, edit: func(c *canaryv1.Canary) { c.Spec.CanaryService = c.Spec.StableService }},
		{name: "error rate bounded", steps: []int{100}, edit: func(c *canaryv1.Canary) { c.Spec.SLO.ErrorRateMax = 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validCanary("http://prometheus.example")
			candidate.Spec.Steps = tc.steps
			if tc.edit != nil {
				tc.edit(candidate)
			}
			if err := validateSpec(candidate, effectiveSteps(candidate.Spec.Steps), effectiveInterval(candidate.Spec.StepInterval.Duration)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestReconcileProgressionCadenceAndRelativeRollback(t *testing.T) {
	p95, errorRate := "100", "0.001"
	server := metricServer(t, &p95, &errorRate)
	defer server.Close()
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	cn := validCanary(server.URL)
	cn.Status = canaryv1.CanaryStatus{Phase: "Pending", LastTransition: metav1.NewTime(now), ObservedGeneration: 1}
	scheme := testScheme(t)
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&canaryv1.Canary{}).WithObjects(cn, service("stable", "100"), service("canary", "0")).Build()
	r := &CanaryReconciler{Client: baseClient, Scheme: scheme, HTTPClient: server.Client(), Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"}}

	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("unexpected first requeue: %s", result.RequeueAfter)
	}
	current := &canaryv1.Canary{}
	if err := baseClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.CurrentWeight != 10 || current.Status.CurrentStepIndex != 1 || !current.Status.BaselineCaptured || current.Status.BaselineP95Ms != 100 {
		t.Fatalf("unexpected first-step status: %+v", current.Status)
	}

	now = now.Add(10 * time.Second)
	result, err = r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 20*time.Second {
		t.Fatalf("early event should wait 20s, got %s", result.RequeueAfter)
	}
	if err := baseClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.CurrentStepIndex != 1 {
		t.Fatal("early reconciliation advanced the rollout")
	}

	now = now.Add(21 * time.Second)
	p95 = "250"
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != "Failed" || current.Status.CurrentWeight != 0 || !strings.Contains(current.Status.Message, "increased") {
		t.Fatalf("relative p95 breach did not roll back: %+v", current.Status)
	}
	stable := &corev1.Service{}
	canary := &corev1.Service{}
	_ = baseClient.Get(context.Background(), types.NamespacedName{Name: "stable", Namespace: "default"}, stable)
	_ = baseClient.Get(context.Background(), types.NamespacedName{Name: "canary", Namespace: "default"}, canary)
	if stable.Annotations["canary.example.io/weight"] != "100" || canary.Annotations["canary.example.io/weight"] != "0" {
		t.Fatalf("rollback weights are wrong: stable=%v canary=%v", stable.Annotations, canary.Annotations)
	}
}

type failingPatchClient struct {
	client.Client
	failName string
}

func (c *failingPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetName() == c.failName {
		return fmt.Errorf("synthetic patch failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestServiceUpdateCompensatesOnSecondPatchFailure(t *testing.T) {
	scheme := testScheme(t)
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service("stable", "70"), service("canary", "30")).Build()
	r := &CanaryReconciler{Client: &failingPatchClient{Client: baseClient, failName: "canary"}}
	cn := validCanary("http://prometheus.example")
	if err := r.applyServiceWeights(context.Background(), cn, 50, 50); err == nil {
		t.Fatal("expected second patch to fail")
	}
	stable := &corev1.Service{}
	if err := baseClient.Get(context.Background(), types.NamespacedName{Name: "stable", Namespace: "default"}, stable); err != nil {
		t.Fatal(err)
	}
	if stable.Annotations["canary.example.io/weight"] != "70" {
		t.Fatalf("stable service was not restored: %v", stable.Annotations)
	}
	if len(stable.OwnerReferences) != 0 {
		t.Fatal("controller claimed ownership of an existing service")
	}
}

func TestFetchSLOsRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "http status", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }},
		{name: "malformed json", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }},
		{name: "missing sample", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}},
		{name: "non finite", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"value":[1,"NaN"]}]}}`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			cn := validCanary(server.URL)
			r := &CanaryReconciler{HTTPClient: server.Client()}
			if _, _, err := r.fetchSLOs(context.Background(), cn); err == nil {
				t.Fatal("expected response to be rejected")
			}
		})
	}
	cn := validCanary("file:///tmp/prometheus")
	if _, _, err := (&CanaryReconciler{HTTPClient: http.DefaultClient}).fetchSLOs(context.Background(), cn); err == nil {
		t.Fatal("expected non-HTTP URL to be rejected")
	}
	if _, _, err := (&CanaryReconciler{}).fetchSLOs(context.Background(), validCanary("http://prometheus.example")); err == nil {
		t.Fatal("expected nil client to be rejected")
	}
}
