package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	canaryv1 "github.com/example/canary-operator/pkg/apis/canary/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultStepInterval = 30 * time.Second
	longRequeue         = 2 * time.Minute
)

var defaultSteps = []int{10, 50, 100}

type CanaryReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	HTTPClient *http.Client
	Now        func() time.Time
}

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
	originalStatus := cn.DeepCopy().Status

	steps := effectiveSteps(cn.Spec.Steps)
	interval := effectiveInterval(cn.Spec.StepInterval.Duration)
	if err := validateSpec(cn, steps, interval); err != nil {
		if cn.Status.Phase == "Invalid" && cn.Status.ObservedGeneration == cn.Generation && cn.Status.Message == err.Error() {
			return ctrl.Result{}, nil
		}
		transitioned := cn.Status.Phase != "Invalid" || cn.Status.ObservedGeneration != cn.Generation
		if cn.Status.RolloutConfigHash != "" {
			stableService, canaryService, ok := activeServices(cn)
			if ok {
				if rollbackErr := r.applyServiceWeightsForServices(ctx, cn.Namespace, stableService, canaryService, 100, 0); rollbackErr != nil {
					cn.Status.Phase = "Invalid"
					cn.Status.Message = fmt.Sprintf("Invalid spec; rollback failed: %v", rollbackErr)
					cn.Status.ObservedGeneration = cn.Generation
					if transitioned {
						cn.Status.LastTransition = metav1.NewTime(r.now())
					}
					setCondition(cn, metav1.ConditionFalse, "RollbackFailed", cn.Status.Message)
					if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
						return ctrl.Result{}, errors.Join(rollbackErr, statusErr)
					}
					return ctrl.Result{}, rollbackErr
				}
				cn.Status.CurrentWeight = 0
			}
		}
		cn.Status.Phase = "Invalid"
		cn.Status.Message = err.Error()
		cn.Status.ObservedGeneration = cn.Generation
		if transitioned {
			cn.Status.LastTransition = metav1.NewTime(r.now())
		}
		setCondition(cn, metav1.ConditionFalse, "InvalidSpec", err.Error())
		if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		r.eventf(cn, corev1.EventTypeWarning, "InvalidSpec", "%v", err)
		return ctrl.Result{}, nil
	}

	configHash := rolloutConfigHash(cn, steps)
	checkpointConfig := false
	if cn.Status.RolloutConfigHash == "" {
		cn.Status.RolloutConfigHash = configHash
		cn.Status.ActiveStableService = cn.Spec.StableService
		cn.Status.ActiveCanaryService = cn.Spec.CanaryService
		checkpointConfig = cn.Status.Phase != ""
	} else if cn.Status.RolloutConfigHash != configHash {
		stableService, canaryService, ok := activeServices(cn)
		if ok {
			if err := r.applyServiceWeightsForServices(ctx, cn.Namespace, stableService, canaryService, 100, 0); err != nil {
				cn.Status.Message = fmt.Sprintf("Rollback failed before rollout restart: %v", err)
				setCondition(cn, metav1.ConditionFalse, "RollbackFailed", cn.Status.Message)
				if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
					return ctrl.Result{}, errors.Join(err, statusErr)
				}
				return ctrl.Result{}, err
			}
		}
		cn.Status = canaryv1.CanaryStatus{
			Phase:               "Pending",
			LastTransition:      metav1.NewTime(r.now()),
			Message:             "Rollout configuration changed; waiting for a new baseline",
			ObservedGeneration:  cn.Generation,
			RolloutConfigHash:   configHash,
			ActiveStableService: cn.Spec.StableService,
			ActiveCanaryService: cn.Spec.CanaryService,
		}
		setCondition(cn, metav1.ConditionFalse, "Pending", cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cn.Status.ObservedGeneration = cn.Generation

	if cn.Status.Phase == "" {
		cn.Status.Phase = "Pending"
		cn.Status.LastTransition = metav1.NewTime(r.now())
		cn.Status.Message = "Waiting for initial SLO sample"
		setCondition(cn, metav1.ConditionFalse, "Pending", cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if checkpointConfig {
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	p95, errRate, err := r.fetchSLOs(ctx, cn)
	if err != nil {
		logger.Error(err, "failed fetching SLO metrics")
		cn.Status.Message = "Prometheus query failed"
		setCondition(cn, metav1.ConditionFalse, "PromQueryError", cn.Status.Message)
		if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
			return ctrl.Result{}, errors.Join(err, statusErr)
		}
		r.eventf(cn, corev1.EventTypeWarning, "PromQueryError", "Failed to fetch SLOs: %v", err)
		return ctrl.Result{}, err
	}
	cn.Status.P95LatencyMs = p95
	cn.Status.ErrorRate = errRate
	if !cn.Status.BaselineCaptured {
		cn.Status.BaselineP95Ms = p95
		cn.Status.BaselineCaptured = true
		cn.Status.ErrorBudgetRem = errorBudgetRemaining(errRate, cn.Spec.SLO.ErrorRateMax)
		cn.Status.Message = "Initial SLO baseline captured"
		setCondition(cn, metav1.ConditionFalse, "BaselineCaptured", cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	cn.Status.ErrorBudgetRem = errorBudgetRemaining(errRate, cn.Spec.SLO.ErrorRateMax)

	if reason, breached := breachReason(cn, p95, errRate); breached {
		if err := r.applyServiceWeights(ctx, cn, 100, 0); err != nil {
			cn.Status.Message = fmt.Sprintf("Rollback failed after SLO breach: %v", err)
			setCondition(cn, metav1.ConditionFalse, "RollbackFailed", cn.Status.Message)
			if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
				return ctrl.Result{}, errors.Join(err, statusErr)
			}
			return ctrl.Result{}, err
		}
		cn.Status.Phase = "Failed"
		cn.Status.CurrentWeight = 0
		cn.Status.Message = "Rollback: " + reason
		cn.Status.LastTransition = metav1.NewTime(r.now())
		setCondition(cn, metav1.ConditionFalse, "SLOBreach", cn.Status.Message)
		r.eventf(cn, corev1.EventTypeWarning, "Rollback", "%s", cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	if cn.Status.Phase == "Failed" || cn.Status.Phase == "Succeeded" {
		setCondition(cn, conditionStatus(cn.Status.Phase == "Succeeded"), cn.Status.Phase, cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	if cn.Status.Phase == "Progressing" {
		nextStepAt := cn.Status.LastTransition.Time.Add(interval)
		if remaining := nextStepAt.Sub(r.now()); remaining > 0 {
			setCondition(cn, metav1.ConditionFalse, "WaitingForStep", "Waiting for the configured step interval")
			if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	if cn.Status.CurrentStepIndex >= len(steps) {
		cn.Status.Phase = "Succeeded"
		cn.Status.Message = "Reached final weight"
		cn.Status.LastTransition = metav1.NewTime(r.now())
		setCondition(cn, metav1.ConditionTrue, "Succeeded", cn.Status.Message)
		if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}

	targetWeight := steps[cn.Status.CurrentStepIndex]
	if err := r.applyServiceWeights(ctx, cn, 100-targetWeight, targetWeight); err != nil {
		cn.Status.Message = fmt.Sprintf("Traffic update failed: %v", err)
		setCondition(cn, metav1.ConditionFalse, "TrafficUpdateFailed", cn.Status.Message)
		if statusErr := r.updateStatus(ctx, cn, originalStatus); statusErr != nil {
			return ctrl.Result{}, errors.Join(err, statusErr)
		}
		return ctrl.Result{}, err
	}

	cn.Status.CurrentWeight = targetWeight
	cn.Status.CurrentStepIndex++
	cn.Status.LastTransition = metav1.NewTime(r.now())
	if targetWeight == 100 {
		cn.Status.Phase = "Succeeded"
		cn.Status.Message = "Canary completed"
		setCondition(cn, metav1.ConditionTrue, "Succeeded", cn.Status.Message)
	} else {
		cn.Status.Phase = "Progressing"
		cn.Status.Message = fmt.Sprintf("Shifted canary to %d%%", targetWeight)
		setCondition(cn, metav1.ConditionFalse, "Progressing", cn.Status.Message)
	}
	r.eventf(cn, corev1.EventTypeNormal, "Progress", "%s", cn.Status.Message)
	if err := r.updateStatus(ctx, cn, originalStatus); err != nil {
		return ctrl.Result{}, err
	}
	if targetWeight == 100 {
		return ctrl.Result{RequeueAfter: longRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func effectiveSteps(configured []int) []int {
	if len(configured) == 0 {
		return append([]int(nil), defaultSteps...)
	}
	return append([]int(nil), configured...)
}

func effectiveInterval(configured time.Duration) time.Duration {
	if configured == 0 {
		return defaultStepInterval
	}
	return configured
}

func rolloutConfigHash(cn *canaryv1.Canary, steps []int) string {
	payload, _ := json.Marshal(struct {
		TargetRef     string `json:"targetRef"`
		StableService string `json:"stableService"`
		CanaryService string `json:"canaryService"`
		Steps         []int  `json:"steps"`
	}{
		TargetRef:     cn.Spec.TargetRef,
		StableService: cn.Spec.StableService,
		CanaryService: cn.Spec.CanaryService,
		Steps:         steps,
	})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func activeServices(cn *canaryv1.Canary) (string, string, bool) {
	stableService := cn.Status.ActiveStableService
	canaryService := cn.Status.ActiveCanaryService
	if stableService == "" || canaryService == "" || stableService == canaryService {
		return "", "", false
	}
	return stableService, canaryService, true
}

func validateSpec(cn *canaryv1.Canary, steps []int, interval time.Duration) error {
	if cn.Spec.TargetRef == "" {
		return errors.New("targetRef is required")
	}
	if cn.Spec.StableService == "" || cn.Spec.CanaryService == "" {
		return errors.New("stableService and canaryService are required")
	}
	if cn.Spec.StableService == cn.Spec.CanaryService {
		return errors.New("stableService and canaryService must differ")
	}
	if interval <= 0 {
		return errors.New("stepInterval must be positive")
	}
	previous := 0
	for _, step := range steps {
		if step < 1 || step > 100 {
			return fmt.Errorf("step %d must be between 1 and 100", step)
		}
		if step <= previous {
			return errors.New("steps must be strictly increasing")
		}
		previous = step
	}
	if len(steps) == 0 || steps[len(steps)-1] != 100 {
		return errors.New("steps must end at 100")
	}
	if cn.Spec.SLO.P95LatencyMsMax < 0 || cn.Spec.SLO.ErrorRateMax < 0 || cn.Spec.SLO.ErrorRateMax > 1 {
		return errors.New("SLO limits must be non-negative and errorRateMax cannot exceed 1")
	}
	if cn.Spec.Abort.MinErrorBudgetPercent < 0 || cn.Spec.Abort.MinErrorBudgetPercent > 100 || cn.Spec.Abort.MaxP95IncreaseMs < 0 {
		return errors.New("abort limits must be non-negative and percentages cannot exceed 100")
	}
	if cn.Spec.SLO.P95LatencyMsMax > 0 && cn.Spec.SLO.P95LatencyQuery == "" {
		return errors.New("p95LatencyQuery is required when p95LatencyMsMax is set")
	}
	if cn.Spec.SLO.ErrorRateMax > 0 && cn.Spec.SLO.ErrorRateQuery == "" {
		return errors.New("errorRateQuery is required when errorRateMax is set")
	}
	return nil
}

func errorBudgetRemaining(errorRate, maximum float64) float64 {
	if maximum <= 0 {
		return 100
	}
	remaining := (1 - errorRate/maximum) * 100
	if remaining < 0 {
		return 0
	}
	if remaining > 100 {
		return 100
	}
	return remaining
}

func breachReason(cn *canaryv1.Canary, p95, errorRate float64) (string, bool) {
	if max := cn.Spec.SLO.P95LatencyMsMax; max > 0 && p95 > max {
		return fmt.Sprintf("p95 %.2fms exceeds %.2fms", p95, max), true
	}
	if max := cn.Spec.SLO.ErrorRateMax; max > 0 && errorRate > max {
		return fmt.Sprintf("error rate %.4f exceeds %.4f", errorRate, max), true
	}
	if min := cn.Spec.Abort.MinErrorBudgetPercent; min > 0 && cn.Status.ErrorBudgetRem < min {
		return fmt.Sprintf("error budget %.2f%% is below %.2f%%", cn.Status.ErrorBudgetRem, min), true
	}
	if maxIncrease := cn.Spec.Abort.MaxP95IncreaseMs; cn.Status.BaselineCaptured && maxIncrease > 0 && p95-cn.Status.BaselineP95Ms > maxIncrease {
		return fmt.Sprintf("p95 increased %.2fms over baseline", p95-cn.Status.BaselineP95Ms), true
	}
	return "", false
}

type annotationValue struct {
	value   string
	present bool
}

func (r *CanaryReconciler) applyServiceWeights(ctx context.Context, cn *canaryv1.Canary, stableWeight, canaryWeight int) error {
	return r.applyServiceWeightsForServices(ctx, cn.Namespace, cn.Spec.StableService, cn.Spec.CanaryService, stableWeight, canaryWeight)
}

func (r *CanaryReconciler) applyServiceWeightsForServices(ctx context.Context, namespace, stableService, canaryService string, stableWeight, canaryWeight int) error {
	stablePrevious, err := r.patchServiceWeight(ctx, namespace, stableService, stableWeight)
	if err != nil {
		return fmt.Errorf("update stable service: %w", err)
	}
	if _, err := r.patchServiceWeight(ctx, namespace, canaryService, canaryWeight); err != nil {
		rollbackErr := r.restoreServiceWeight(ctx, namespace, stableService, stablePrevious)
		if rollbackErr != nil {
			return errors.Join(fmt.Errorf("update canary service: %w", err), fmt.Errorf("restore stable service: %w", rollbackErr))
		}
		return fmt.Errorf("update canary service: %w; stable service restored", err)
	}
	return nil
}

func (r *CanaryReconciler) updateStatus(ctx context.Context, cn *canaryv1.Canary, before canaryv1.CanaryStatus) error {
	if apiequality.Semantic.DeepEqual(before, cn.Status) {
		return nil
	}
	return r.Status().Update(ctx, cn)
}

func (r *CanaryReconciler) patchServiceWeight(ctx context.Context, namespace, name string, weight int) (annotationValue, error) {
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := r.Get(ctx, key, svc); err != nil {
		return annotationValue{}, err
	}
	previous := annotationValue{}
	if svc.Annotations != nil {
		previous.value, previous.present = svc.Annotations["canary.example.io/weight"]
	}
	before := svc.DeepCopy()
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations["canary.example.io/weight"] = strconv.Itoa(weight)
	if err := r.Patch(ctx, svc, client.MergeFrom(before)); err != nil {
		return annotationValue{}, err
	}
	return previous, nil
}

func (r *CanaryReconciler) restoreServiceWeight(ctx context.Context, namespace, name string, previous annotationValue) error {
	svc := &corev1.Service{}
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := r.Get(ctx, key, svc); err != nil {
		return err
	}
	before := svc.DeepCopy()
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	if previous.present {
		svc.Annotations["canary.example.io/weight"] = previous.value
	} else {
		delete(svc.Annotations, "canary.example.io/weight")
	}
	return r.Patch(ctx, svc, client.MergeFrom(before))
}

func (r *CanaryReconciler) fetchSLOs(ctx context.Context, cn *canaryv1.Canary) (float64, float64, error) {
	if r.HTTPClient == nil {
		return 0, 0, errors.New("HTTP client is not configured")
	}
	baseURL := cn.Spec.SLO.PrometheusURL
	if baseURL == "" {
		baseURL = os.Getenv("PROMETHEUS_URL")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return 0, 0, fmt.Errorf("invalid Prometheus URL %q", baseURL)
	}
	query := func(expression string) (float64, error) {
		if expression == "" {
			return 0, nil
		}
		u := *parsed
		u.Path = "/api/v1/query"
		values := u.Query()
		values.Set("query", expression)
		u.RawQuery = values.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return 0, err
		}
		resp, err := r.HTTPClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, fmt.Errorf("Prometheus returned HTTP %d", resp.StatusCode)
		}
		var payload struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return 0, fmt.Errorf("decode Prometheus response: %w", err)
		}
		if payload.Status != "success" || len(payload.Data.Result) == 0 || len(payload.Data.Result[0].Value) < 2 {
			return 0, errors.New("Prometheus response contains no sample")
		}
		raw, ok := payload.Data.Result[0].Value[1].(string)
		if !ok {
			return 0, errors.New("Prometheus sample is not a string")
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("invalid Prometheus sample %q", raw)
		}
		return value, nil
	}
	p95, err := query(cn.Spec.SLO.P95LatencyQuery)
	if err != nil {
		return 0, 0, err
	}
	errorRate, err := query(cn.Spec.SLO.ErrorRateQuery)
	if err != nil {
		return 0, 0, err
	}
	return p95, errorRate, nil
}

func (r *CanaryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *CanaryReconciler) eventf(cn *canaryv1.Canary, eventType, reason, message string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(cn, eventType, reason, message, args...)
	}
}

func setCondition(cn *canaryv1.Canary, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&cn.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: cn.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func (r *CanaryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&canaryv1.Canary{}).
		WithOptions(controller.Options{RateLimiter: NewDefaultRateLimiter()}).
		Complete(r)
}
