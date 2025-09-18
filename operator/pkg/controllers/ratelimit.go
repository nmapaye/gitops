package controllers

import (
    "time"

    "k8s.io/client-go/util/workqueue"
)

// NewDefaultRateLimiter returns a named item/exponential rate limiter to avoid hot-looping.
func NewDefaultRateLimiter() workqueue.RateLimiter {
    return workqueue.NewItemExponentialFailureRateLimiter(1*time.Second, 30*time.Second)
}

