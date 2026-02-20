package api

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var claimsCreatedTotal = promauto.With(metrics.Registry).NewCounter(prometheus.CounterOpts{
	Name: "claim_controller_claims_created_total",
	Help: "Total number of claims successfully created.",
})

var claimReadyDurationSeconds = promauto.With(metrics.Registry).NewHistogram(prometheus.HistogramOpts{
	Name:    "claim_controller_claim_ready_duration_seconds",
	Help:    "Time in seconds from claim creation to healthy state.",
	Buckets: prometheus.ExponentialBuckets(1, 2, 8),
})

var claimsReleasedTotal = promauto.With(metrics.Registry).NewCounter(prometheus.CounterOpts{
	Name: "claim_controller_claims_released_total",
	Help: "Total number of claims successfully released.",
})

var timedOutClaimsTotal = promauto.With(metrics.Registry).NewCounter(prometheus.CounterOpts{
	Name: "claim_controller_timedout_claims_total",
	Help: "Total number of claims that timed out waiting for readiness.",
})

var claimLifetimeExpectedRatio = promauto.With(metrics.Registry).NewHistogram(prometheus.HistogramOpts{
	Name:    "claim_controller_claim_lifetime_expected_ratio",
	Help:    "Ratio between actual claim lifetime and expected lifetime at deletion.",
	Buckets: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1, 1.1, 2, 3},
})


func newClaimLifetimeDurationHistogram(defaultTTL time.Duration) prometheus.Observer {
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "claim_controller_claim_lifetime_duration_seconds",
		Help:    "Claim lifetime in seconds from creation to release.",
		Buckets: claimLifetimeDurationBuckets(defaultTTL),
	})

	err := metrics.Registry.Register(histogram)
	if err == nil {
		return histogram
	}

	var alreadyRegistered prometheus.AlreadyRegisteredError
	if errors.As(err, &alreadyRegistered) {
		existingHistogram, ok := alreadyRegistered.ExistingCollector.(prometheus.Observer)
		if ok {
			return existingHistogram
		}
	}

	panic(fmt.Errorf("register claim lifetime duration histogram: %w", err))
}

func claimLifetimeDurationBuckets(defaultTTL time.Duration) []float64 {
	ttlSeconds := defaultTTL.Seconds()
	if ttlSeconds <= 0 {
		ttlSeconds = 180
	}

	const maxBuckets = 10
	buckets := make([]float64, 0, maxBuckets)
	for i := 1; i <= maxBuckets; i++ {
		buckets = append(buckets, ttlSeconds*float64(i)/maxBuckets)
	}

	sort.Float64s(buckets)
	uniqueBuckets := make([]float64, 0, len(buckets))
	for _, bucket := range buckets {
		if len(uniqueBuckets) == 0 || uniqueBuckets[len(uniqueBuckets)-1] != bucket {
			uniqueBuckets = append(uniqueBuckets, bucket)
		}
	}

	if len(uniqueBuckets) > maxBuckets {
		return uniqueBuckets[:maxBuckets]
	}

	return uniqueBuckets
}

func newClaimTotalDurationHistogram(maxTTL time.Duration) prometheus.Observer {
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "claim_controller_claim_total_duration_seconds",
		Help:    "Configured total claim duration in seconds from creation to expiration.",
		Buckets: claimTotalDurationBuckets(maxTTL),
	})

	err := metrics.Registry.Register(histogram)
	if err == nil {
		return histogram
	}

	var alreadyRegistered prometheus.AlreadyRegisteredError
	if errors.As(err, &alreadyRegistered) {
		existingHistogram, ok := alreadyRegistered.ExistingCollector.(prometheus.Observer)
		if ok {
			return existingHistogram
		}
	}

	panic(fmt.Errorf("register claim total duration histogram: %w", err))
}

func claimTotalDurationBuckets(maxTTL time.Duration) []float64 {
	if maxTTL <= 0 {
		maxTTL = 10 * time.Minute
	}

	maxSeconds := maxTTL.Seconds()
	stepSeconds := time.Minute.Seconds()
	buckets := make([]float64, 0, int(maxSeconds/stepSeconds)+1)

	for bucket := stepSeconds; bucket <= maxSeconds; bucket += stepSeconds {
		buckets = append(buckets, bucket)
	}

	if len(buckets) == 0 || buckets[len(buckets)-1] < maxSeconds {
		buckets = append(buckets, maxSeconds)
	}

	return buckets
}
