package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/util/retry"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/values"
	"github.com/prometheus/client_golang/prometheus"
)

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
	if err := s.waitForClaimReady(r.Context(), claimName, 120*time.Second); err != nil {
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}
