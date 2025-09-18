package controllers

import (
    "context"
    "encoding/json"
    "fmt"
    "os"
    "net/http"
    "net/url"
    "strconv"
    "time"

    canaryv1 "github.com/example/canary-operator/pkg/apis/canary/v1alpha1"
    corev1 "k8s.io/api/core/v1"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/controller"
    "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
    "sigs.k8s.io/controller-runtime/pkg/log"
)

type CanaryReconciler struct {
    client.Client
    Scheme     *runtime.Scheme
    Recorder   ctrl.EventRecorder
    HTTPClient *http.Client
}

// RBAC permissions
// +kubebuilder:rbac:groups=canary.example.io,resources=canaries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=canary.example.io,resources=canaries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch

func (r *CanaryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)
    cn := &canaryv1.Canary{}
    if err := r.Get(ctx, req.NamespacedName, cn); err != nil {
        if apierrors.IsNotFound(err) {
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    // Defaulting
    if len(cn.Spec.Steps) == 0 {
        cn.Spec.Steps = []int{10, 50, 100}
    }
    if cn.Spec.StepInterval.Duration == 0 {
        cn.Spec.StepInterval = metav1.Duration{Duration: 30 * time.Second}
    }

    // Initialize status
    if cn.Status.Phase == "" {
        cn.Status.Phase = "Pending"
        cn.Status.LastTransition = metav1.Now()
        if err := r.Status().Update(ctx, cn); err != nil {
            return ctrl.Result{}, err
        }
        return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
    }

    // Fetch SLO metrics from Prometheus
    p95, errRate, err := r.fetchSLOs(ctx, cn)
    if err != nil {
        logger.Error(err, "failed fetching SLO metrics")
        r.Recorder.Eventf(cn, corev1.EventTypeWarning, "PromQueryError", "Failed to fetch SLOs: %v", err)
        // transient error, retry with rate limiter
        return ctrl.Result{}, err
    }
    cn.Status.P95LatencyMs = p95
    cn.Status.ErrorRate = errRate

    // Compute a simplistic error budget remaining (1 - errorRate/errorRateMax)
    if cn.Spec.SLO.ErrorRateMax > 0 {
        rem := 1 - (errRate / cn.Spec.SLO.ErrorRateMax)
        if rem < 0 {
            rem = 0
        }
        cn.Status.ErrorBudgetRem = rem * 100
    }

    // Decide next action
    // Abort if SLOs breached
    if (cn.Spec.SLO.P95LatencyMsMax > 0 && p95 > cn.Spec.SLO.P95LatencyMsMax) ||
        (cn.Spec.SLO.ErrorRateMax > 0 && errRate > cn.Spec.SLO.ErrorRateMax) ||
        (cn.Spec.Abort.MinErrorBudgetPercent > 0 && cn.Status.ErrorBudgetRem < cn.Spec.Abort.MinErrorBudgetPercent) {
        // rollback
        if err := r.applyServiceWeights(ctx, cn, 100, 0); err != nil {
            return ctrl.Result{}, err
        }
        cn.Status.Phase = "Failed"
        cn.Status.Message = fmt.Sprintf("Rollback: p95=%.2fms errRate=%.4f", p95, errRate)
        cn.Status.LastTransition = metav1.Now()
        r.Recorder.Eventf(cn, corev1.EventTypeWarning, "Rollback", cn.Status.Message)
        if err := r.Status().Update(ctx, cn); err != nil {
            return ctrl.Result{}, err
        }
        // stop reconciling aggressively; next updates will be via changes
        return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
    }

    // Progress or finish
    if cn.Status.CurrentStepIndex >= len(cn.Spec.Steps) {
        // already finished
        cn.Status.Phase = "Succeeded"
        cn.Status.Message = "Reached final weight"
        cn.Status.LastTransition = metav1.Now()
        _ = r.Status().Update(ctx, cn)
        return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
    }

    // Apply next weight
    targetWeight := cn.Spec.Steps[cn.Status.CurrentStepIndex]
    if err := r.applyServiceWeights(ctx, cn, 100-targetWeight, targetWeight); err != nil {
        return ctrl.Result{}, err
    }
    cn.Status.CurrentWeight = targetWeight
    cn.Status.Phase = "Progressing"
    cn.Status.Message = fmt.Sprintf("Shifted canary to %d%%", targetWeight)
    cn.Status.LastTransition = metav1.Now()
    r.Recorder.Eventf(cn, corev1.EventTypeNormal, "Progress", cn.Status.Message)
    if err := r.Status().Update(ctx, cn); err != nil {
        return ctrl.Result{}, err
    }

    // Move to next step on next reconcile
    if targetWeight == 100 {
        cn.Status.Phase = "Succeeded"
        cn.Status.Message = "Canary completed"
        _ = r.Status().Update(ctx, cn)
        return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
    }
    cn.Status.CurrentStepIndex++
    _ = r.Status().Update(ctx, cn)
    return ctrl.Result{RequeueAfter: cn.Spec.StepInterval.Duration}, nil
}

// applyServiceWeights updates annotations on the stable/canary Services to indicate desired traffic weights.
// A service mesh or ingress controller can read these annotations to route traffic accordingly.
func (r *CanaryReconciler) applyServiceWeights(ctx context.Context, cn *canaryv1.Canary, stableWeight, canaryWeight int) error {
    // Helper to patch one service
    patchSvc := func(name string, weight int) error {
        if name == "" {
            return nil
        }
        svc := &corev1.Service{}
        key := types.NamespacedName{Name: name, Namespace: cn.Namespace}
        if err := r.Get(ctx, key, svc); err != nil {
            return err
        }
        // ensure map
        if svc.Annotations == nil {
            svc.Annotations = map[string]string{}
        }
        svc.Annotations["canary.example.io/weight"] = strconv.Itoa(weight)
        // set controller ref if not already owned
        if !metav1.IsControlledBy(svc, cn) {
            if err := controllerutil.SetControllerReference(cn, svc, r.Scheme); err != nil {
                // not fatal if we can't own (e.g., existing service), proceed with patch
            }
        }
        return r.Update(ctx, svc)
    }
    if err := patchSvc(cn.Spec.StableService, stableWeight); err != nil {
        return err
    }
    if err := patchSvc(cn.Spec.CanaryService, canaryWeight); err != nil {
        return err
    }
    return nil
}

func (r *CanaryReconciler) fetchSLOs(ctx context.Context, cn *canaryv1.Canary) (p95 float64, errRate float64, err error) {
    if cn.Spec.SLO.PrometheusURL == "" {
        if env := os.Getenv("PROMETHEUS_URL"); env != "" {
            cn.Spec.SLO.PrometheusURL = env
        } else {
            return 0, 0, fmt.Errorf("prometheusURL not set")
        }
    }
    query := func(q string) (float64, error) {
        if q == "" {
            return 0, nil
        }
        u, _ := url.Parse(cn.Spec.SLO.PrometheusURL)
        u.Path = "/api/v1/query"
        qs := url.Values{}
        qs.Set("query", q)
        u.RawQuery = qs.Encode()
        req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
        resp, err := r.HTTPClient.Do(req)
        if err != nil {
            return 0, err
        }
        defer resp.Body.Close()
        var payload struct {
            Status string `json:"status"`
            Data   struct {
                ResultType string `json:"resultType"`
                Result     []struct {
                    Value [2]any `json:"value"`
                } `json:"result"`
            } `json:"data"`
        }
        if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
            return 0, err
        }
        if payload.Status != "success" || len(payload.Data.Result) == 0 {
            return 0, fmt.Errorf("no data")
        }
        strv, _ := payload.Data.Result[0].Value[1].(string)
        return strconv.ParseFloat(strv, 64)
    }
    p95, err = query(cn.Spec.SLO.P95LatencyQuery)
    if err != nil {
        return 0, 0, err
    }
    errRate, err = query(cn.Spec.SLO.ErrorRateQuery)
    if err != nil {
        return 0, 0, err
    }
    return p95, errRate, nil
}

func (r *CanaryReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&canaryv1.Canary{}).
        WithOptions(controller.Options{RateLimiter: NewDefaultRateLimiter()}).
        Complete(r)
}
