package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/template"
	"github.com/nonot/claim-controller/internal/values"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

type Config struct {
	Namespace      string
	DefaultTTL     time.Duration
	MaxTTL         time.Duration
	TemplatePath   string
	ValuesProvider values.Provider
	Client         client.Client
}

type Server struct {
	namespace      string
	defaultTTL     time.Duration
	maxTTL         time.Duration
	templatePath   string
	valuesProvider values.Provider
	client         client.Client
	claimLifetime  prometheus.Observer
	claimTotalTTL  prometheus.Observer
	mux            *http.ServeMux
}

type claimRequest struct {
	TTL string `json:"ttl"`
}

func NewServer(cfg Config) *Server {
	maxTTL := cfg.MaxTTL
	if maxTTL <= 0 {
		maxTTL = cfg.DefaultTTL
	}
	if maxTTL < cfg.DefaultTTL {
		maxTTL = cfg.DefaultTTL
	}

	s := &Server{
		namespace:      cfg.Namespace,
		defaultTTL:     cfg.DefaultTTL,
		maxTTL:         maxTTL,
		templatePath:   cfg.TemplatePath,
		valuesProvider: cfg.ValuesProvider,
		client:         cfg.Client,
		claimLifetime:  newClaimLifetimeDurationHistogram(cfg.DefaultTTL),
		claimTotalTTL:  newClaimTotalDurationHistogram(maxTTL),
		mux:            http.NewServeMux(),
	}
	s.routes()
	return s
}

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

func (s *Server) Start(ctx context.Context) error {
	if s.valuesProvider == nil {
		return nil
	}
	return s.valuesProvider.Start(ctx)
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/claim", s.handleClaim)
	s.mux.HandleFunc("/release/{id}", s.handleRelease)
	s.mux.HandleFunc("/renew/{id}", s.handleRenew)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ttl, err := s.ttlFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	claimID := randomSuffix(8)
	claimName := fmt.Sprintf("claim-%s", claimID)
	expiresAt := time.Now().UTC().Add(ttl)

	resourceTemplate, err := s.loadResourceTemplate(claimID)
	// add id to returned payload
	if err != nil {
		log.Printf("failed to load resource template: %v", err)
		http.Error(w, "failed to render templates", http.StatusInternalServerError)
		return
	}
	if len(resourceTemplate.RenderedObjects) == 0 {
		http.Error(w, "rendered templates must include at least one resource", http.StatusInternalServerError)
		return
	}

	renderedResourcesBytes, err := json.Marshal(resourceTemplate.RenderedObjects)
	if err != nil {
		http.Error(w, "failed to serialize rendered resources", http.StatusInternalServerError)
		return
	}

	claim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: s.namespace,
			Labels: map[string]string{
				controller.ManagedByLabelKey: controller.ManagedByLabelValue,
				controller.ClaimLabelKey:     claimName,
				controller.ClaimLabelKeyId:   claimID,
			},
			Annotations: map[string]string{
				controller.ExpiresAtAnnotationKey: expiresAt.Format(time.RFC3339),
				controller.CreatedByAnnotationKey: controller.CreatedByAnnotationValue,
			},
		},
		Data: map[string]string{
			controller.RenderedResourcesDataKey:    string(renderedResourcesBytes),
			controller.ClaimStatusDataKey:          "pending",
			controller.ClaimStatusMessageDataKey:   "waiting for resources to be created",
			controller.ClaimResourcesStatusDataKey: "[]",
		},
	}

	if ownerRef := s.valuesProvider.GetOwnerReference(); ownerRef != nil {
		claim.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return s.client.Create(ctx, claim)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			http.Error(w, "upstream timeout while creating claim", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "failed to create claim", http.StatusInternalServerError)
		return
	}
	claimsCreatedTotal.Inc()

	readyStart := time.Now()
	if err := s.waitForClaimReady(r.Context(), claimName, 90*time.Second); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			timedOutClaimsTotal.Inc()
			http.Error(w, "timed out waiting for claim resources to become ready", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "failed while waiting for claim readiness", http.StatusInternalServerError)
		return
	}
	readyDurationSeconds := time.Since(readyStart).Seconds()
	claimReadyDurationSeconds.Observe(readyDurationSeconds)
	log.Printf("claim became ready: id=%s name=%s ready_duration_seconds=%.6f", claimID, claimName, readyDurationSeconds)

	body := make(map[string]any)
	body["status"] = "ok"
	body["id"] = claimID
	body["expiresAt"] = expiresAt.Format(time.RFC3339)
	body["data"] = resourceTemplate.ReturnValues
	body["releasePath"] = fmt.Sprintf("/release/%s", claimID)
	body["releaseMethod"] = http.MethodPost
	body["renewPath"] = fmt.Sprintf("/renew/%s", claimID)
	body["renewMethod"] = http.MethodPost

	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) ttlFromRequest(r *http.Request) (time.Duration, error) {
	if r.Body == nil {
		return s.defaultTTL, nil
	}

	var req claimRequest
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&req)
	if errors.Is(err, io.EOF) {
		return s.defaultTTL, nil
	}
	if err != nil {
		return 0, fmt.Errorf("invalid request body: %w", err)
	}

	if strings.TrimSpace(req.TTL) == "" {
		return s.defaultTTL, nil
	}

	ttl, err := time.ParseDuration(strings.TrimSpace(req.TTL))
	if err != nil {
		return 0, fmt.Errorf("invalid ttl duration")
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("ttl must be greater than 0")
	}
	if ttl > s.maxTTL {
		return s.maxTTL, nil
	}

	return ttl, nil
}

func (s *Server) waitForClaimReady(ctx context.Context, claimName string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		claim := &corev1.ConfigMap{}
		err := s.client.Get(waitCtx, client.ObjectKey{Namespace: s.namespace, Name: claimName}, claim)
		if err == nil {
			status := strings.TrimSpace(claim.Data[controller.ClaimStatusDataKey])
			if strings.EqualFold(status, "ready") {
				return nil
			}
			if strings.EqualFold(status, "failed") {
				message := strings.TrimSpace(claim.Data[controller.ClaimStatusMessageDataKey])
				if message == "" {
					message = "resource readiness failed"
				}
				return errors.New(message)
			}
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) loadResourceTemplate(claimID string) (template.ResourceTemplate, error) {
	if s.valuesProvider == nil {
		return template.ResourceTemplate{}, errors.New("values provider is not configured")
	}

	valuesData, err := s.valuesProvider.GetValues()
	if err != nil {
		return template.ResourceTemplate{}, err
	}

	return template.LoadResourceTemplateFromValuesData(s.namespace, s.templatePath, valuesData, claimID)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	claimID := strings.TrimSpace(r.PathValue("id"))
	if claimID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	claims, err := s.findManagedClaimsByID(ctx, claimID)
	if err != nil {
		if errors.Is(err, errClaimNotFound) {
			http.Error(w, "claim not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, errClaimNotManaged) {
			http.Error(w, "claim not managed by controller", http.StatusForbidden)
			return
		}
		http.Error(w, "failed to load claim", http.StatusInternalServerError)
		return
	}

	for _, claim := range claims {
		if err := s.client.Delete(ctx, claim.DeepCopy()); err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "claim not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete claim", http.StatusInternalServerError)
			return
		}

		if !claim.CreationTimestamp.IsZero() {
			s.claimLifetime.Observe(time.Since(claim.CreationTimestamp.Time).Seconds())
		}
		if totalDurationSeconds, ok := claimExpectedLifetimeSeconds(claim); ok {
			s.claimTotalTTL.Observe(totalDurationSeconds)
		}
		if ratio, ok := claimLifetimeRatio(claim); ok {
			claimLifetimeExpectedRatio.Observe(ratio)
		}
	}
	claimsReleasedTotal.Add(float64(len(claims)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	claimID := strings.TrimSpace(r.PathValue("id"))
	if claimID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	ttl, err := s.ttlFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	claims, err := s.findManagedClaimsByID(ctx, claimID)
	if err != nil {
		if errors.Is(err, errClaimNotFound) {
			http.Error(w, "claim not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, errClaimNotManaged) {
			http.Error(w, "claim not managed by controller", http.StatusForbidden)
			return
		}
		http.Error(w, "failed to load claim", http.StatusInternalServerError)
		return
	}

	updatedClaim, err := s.renewClaim(ctx, claims[0], ttl)
	if err != nil {
		if errors.Is(err, errMaxTTLReached) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "failed to renew claim", http.StatusInternalServerError)
		return
	}

	body := map[string]any{
		"status":      "ok",
		"id":          claimID,
		"expiresAt":   updatedClaim.Annotations[controller.ExpiresAtAnnotationKey],
		"renewPath":   fmt.Sprintf("/renew/%s", claimID),
		"renewMethod": http.MethodPost,
	}

	writeJSON(w, http.StatusOK, body)
}

var errClaimNotFound = errors.New("claim not found")
var errClaimNotManaged = errors.New("claim not managed by controller")
var errMaxTTLReached = errors.New("max ttl already reached")

func (s *Server) findManagedClaimsByID(ctx context.Context, claimID string) ([]corev1.ConfigMap, error) {

	claimList := &corev1.ConfigMapList{}
	if err := s.client.List(ctx, claimList, client.InNamespace(s.namespace), client.MatchingLabels{controller.ClaimLabelKeyId: claimID}); err != nil {
		log.Printf("failed to list claims: %v", err)
		if apierrors.IsNotFound(err) {
			return nil, errClaimNotFound
		}
		return nil, err
	}
	if len(claimList.Items) == 0 {
		return nil, errClaimNotFound
	}
	for _, claim := range claimList.Items {
		if claim.Labels[controller.ManagedByLabelKey] != controller.ManagedByLabelValue {
			return nil, errClaimNotManaged
		}
	}

	return claimList.Items, nil
}

func (s *Server) renewClaim(ctx context.Context, claim corev1.ConfigMap, ttl time.Duration) (*corev1.ConfigMap, error) {
	now := time.Now().UTC()
	maxExpiresAt := claim.CreationTimestamp.Time.UTC().Add(s.maxTTL)
	if maxExpiresAt.Before(now) || maxExpiresAt.Equal(now) {
		return nil, errMaxTTLReached
	}

	requestedExpiresAt := now.Add(ttl)
	newExpiresAt := requestedExpiresAt
	if newExpiresAt.After(maxExpiresAt) {
		newExpiresAt = maxExpiresAt
	}

	updated := claim.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[controller.ExpiresAtAnnotationKey] = newExpiresAt.Format(time.RFC3339)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.ConfigMap{}
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(updated), current); err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[controller.ExpiresAtAnnotationKey] = newExpiresAt.Format(time.RFC3339)
		return s.client.Update(ctx, current)
	})
	if err != nil {
		return nil, err
	}

	return updated, nil
}

func claimLifetimeRatio(claim corev1.ConfigMap) (float64, bool) {
	if claim.CreationTimestamp.IsZero() {
		return 0, false
	}

	expectedLifetimeSeconds, ok := claimExpectedLifetimeSeconds(claim)
	if !ok || expectedLifetimeSeconds <= 0 {
		return 0, false
	}

	actualLifetime := time.Since(claim.CreationTimestamp.Time)
	if actualLifetime < 0 {
		actualLifetime = 0
	}

	return actualLifetime.Seconds() / expectedLifetimeSeconds, true
}

func claimExpectedLifetimeSeconds(claim corev1.ConfigMap) (float64, bool) {
	if claim.CreationTimestamp.IsZero() {
		return 0, false
	}

	expiresAtRaw := strings.TrimSpace(claim.Annotations[controller.ExpiresAtAnnotationKey])
	if expiresAtRaw == "" {
		return 0, false
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtRaw)
	if err != nil {
		return 0, false
	}

	expectedLifetime := expiresAt.Sub(claim.CreationTimestamp.Time)
	if expectedLifetime <= 0 {
		return 0, false
	}

	return expectedLifetime.Seconds(), true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rnd.Intn(len(letters))]
	}
	return strings.ToLower(string(b))
}
