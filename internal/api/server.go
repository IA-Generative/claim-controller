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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/values"
	"github.com/prometheus/client_golang/prometheus"
)

type Config struct {
	Namespace         string
	DefaultTTL        time.Duration
	MaxTTL            time.Duration
	PreProvisionCount int
	TemplatePath      string
	ValuesProvider    values.Provider
	Client            client.Client
}

type Server struct {
	namespace          string
	defaultTTL         time.Duration
	maxTTL             time.Duration
	templatePath       string
	valuesProvider     values.Provider
	client             client.Client
	claimLifetime      prometheus.Observer
	claimTotalTTL      prometheus.Observer
	claimIdleDuration  prometheus.Observer
	claimUsageDuration prometheus.Observer
	preProvisionCount  int
	mux                *http.ServeMux
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
		namespace:          cfg.Namespace,
		defaultTTL:         cfg.DefaultTTL,
		maxTTL:             maxTTL,
		templatePath:       cfg.TemplatePath,
		valuesProvider:     cfg.ValuesProvider,
		client:             cfg.Client,
		claimLifetime:      newClaimLifetimeDurationHistogram(cfg.DefaultTTL),
		claimTotalTTL:      newClaimTotalDurationHistogram(maxTTL),
		claimIdleDuration:  newClaimIdleDurationHistogram(maxTTL),
		claimUsageDuration: newClaimUsageDurationHistogram(maxTTL),
		preProvisionCount:  max(0, cfg.PreProvisionCount),
		mux:                http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Start(ctx context.Context) error {
	if s.valuesProvider == nil {
		return nil
	}
	if err := s.valuesProvider.Start(ctx); err != nil {
		return err
	}

	if s.preProvisionCount <= 0 {
		return nil
	}

	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if err := s.ensurePreProvisionedClaims(ctx); err != nil {
					log.Printf("failed to ensure pre-provisioned claims: %v", err)
				}
				timer.Reset(15 * time.Second)
			}
		}
	}()

	return nil
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	claim, claimID, expiresAt, isPreProvisioned, err := s.acquireClaim(ctx, ttl)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			http.Error(w, "upstream timeout while creating claim", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "failed to create claim", http.StatusInternalServerError)
		return
	}

	readyStart := time.Now()
	if err := s.waitForClaimReady(r.Context(), claim.Name, 120*time.Second); err != nil {
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
	log.Printf("claim became ready: id=%s name=%s ready_duration_seconds=%.6f", claimID, claim.Name, readyDurationSeconds)

	returnValues := map[string]string{}
	if raw := strings.TrimSpace(claim.Data[controller.ReturnValuesDataKey]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &returnValues)
	}

	body := make(map[string]any)
	body["status"] = "ok"
	body["id"] = claimID
	body["expiresAt"] = expiresAt.Format(time.RFC3339)
	body["data"] = returnValues
	body["releasePath"] = fmt.Sprintf("/release/%s", claimID)
	body["releaseMethod"] = http.MethodPost
	body["renewPath"] = fmt.Sprintf("/renew/%s", claimID)
	body["renewMethod"] = http.MethodPost
	body["preProvisioned"] = isPreProvisioned

	writeJSON(w, http.StatusCreated, body)

	if isPreProvisioned {
		go func() {
			if err := s.ensurePreProvisionedClaims(context.Background()); err != nil {
				log.Printf("failed to replenish pre-provisioned claims: %v", err)
			}
		}()
	}
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

		if totalActualSeconds, ok := claimTotalActualDurationSeconds(claim, time.Now().UTC()); ok {
			s.claimLifetime.Observe(totalActualSeconds)
		}
		if idleSeconds, ok := claimIdleDurationSeconds(claim); ok {
			s.claimIdleDuration.Observe(idleSeconds)
		}
		if usageActualSeconds, ok := claimUsageActualDurationSeconds(claim, time.Now().UTC()); ok {
			s.claimUsageDuration.Observe(usageActualSeconds)
		}
		if totalDurationSeconds, ok := claimExpectedLifetimeSeconds(claim); ok {
			s.claimTotalTTL.Observe(totalDurationSeconds)
		}
		if ratio, ok := claimLifetimeRatio(claim); ok {
			claimLifetimeExpectedRatio.Observe(ratio)
		}
		if usageRatio, ok := claimUsageRatio(claim); ok {
			claimUsageExpectedRatio.Observe(usageRatio)
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
