package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CanarySpec defines the desired state of Canary
type CanarySpec struct {
    // Target deployment reference (name)
    TargetRef string `json:"targetRef"`

    // Stable and Canary services for traffic splitting (by annotation)
    StableService string `json:"stableService"`
    CanaryService string `json:"canaryService"`

    // Progressive steps as percentage weights (e.g., [10,50,100])
    Steps []int `json:"steps,omitempty"`

    // Interval between steps, duration string (e.g., "30s", "2m")
    StepInterval metav1.Duration `json:"stepInterval,omitempty"`

    // SLO config and Prometheus queries
    SLO SLOConfig `json:"slo"`

    // Abort rules for rollback
    Abort AbortRules `json:"abort"`
}

type SLOConfig struct {
    PrometheusURL string `json:"prometheusURL"`
    // Raw queries expected to return vector values
    P95LatencyQuery string  `json:"p95LatencyQuery"`
    ErrorRateQuery  string  `json:"errorRateQuery"`
    P95LatencyMsMax float64 `json:"p95LatencyMsMax"`
    ErrorRateMax    float64 `json:"errorRateMax"`
}

type AbortRules struct {
    // If error budget remaining falls below this percentage ⇒ rollback
    MinErrorBudgetPercent float64 `json:"minErrorBudgetPercent"`
    // Max allowed increase over baseline p95 (ms)
    MaxP95IncreaseMs float64 `json:"maxP95IncreaseMs"`
}

// CanaryStatus defines the observed state of Canary
type CanaryStatus struct {
    Phase             string        `json:"phase,omitempty"`
    CurrentStepIndex  int           `json:"currentStepIndex,omitempty"`
    CurrentWeight     int           `json:"currentWeight,omitempty"`
    LastTransition    metav1.Time   `json:"lastTransition,omitempty"`
    Message           string        `json:"message,omitempty"`
    ErrorBudgetRem    float64       `json:"errorBudgetRemaining,omitempty"`
    P95LatencyMs      float64       `json:"p95LatencyMs,omitempty"`
    ErrorRate         float64       `json:"errorRate,omitempty"`
    BaselineP95Ms     float64       `json:"baselineP95Ms,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cnry
// Canary is the Schema for the canaries API
type Canary struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   CanarySpec   `json:"spec,omitempty"`
    Status CanaryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// CanaryList contains a list of Canary
type CanaryList struct {
    metav1.TypeMeta `json:",inline"`
    metav1.ListMeta `json:"metadata,omitempty"`
    Items           []Canary `json:"items"`
}

