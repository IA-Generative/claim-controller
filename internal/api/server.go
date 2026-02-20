package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/template"
	"github.com/nonot/claim-controller/internal/values"
)

type Config struct {
	Namespace      string
	DefaultTTL     time.Duration
	TemplatePath   string
	ValuesProvider values.Provider
	Client         client.Client
}

type Server struct {
	namespace      string
	defaultTTL     time.Duration
	templatePath   string
	valuesProvider values.Provider
	client         client.Client
	mux            *http.ServeMux
}

func NewServer(cfg Config) *Server {
	s := &Server{
		namespace:      cfg.Namespace,
		defaultTTL:     cfg.DefaultTTL,
		templatePath:   cfg.TemplatePath,
		valuesProvider: cfg.ValuesProvider,
		client:         cfg.Client,
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

	claimID := randomSuffix(8)
	claimName := fmt.Sprintf("claim-%s", claimID)
	expiresAt := time.Now().UTC().Add(s.defaultTTL)

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
			controller.RenderedResourcesDataKey: string(renderedResourcesBytes),
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

	body := make(map[string]any)
	body["id"] = claimID
	body["expiresAt"] = expiresAt.Format(time.RFC3339)
	body["data"] = resourceTemplate.ReturnValues
	body["releasePath"] = fmt.Sprintf("/release/%s", claimID)
	body["releaseMethod"] = http.MethodPost

	writeJSON(w, http.StatusCreated, body)
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

	claimList := &corev1.ConfigMapList{}
	if err := s.client.List(ctx, claimList, client.InNamespace(s.namespace), client.MatchingLabels{controller.ClaimLabelKeyId: claimID}); err != nil {
		log.Printf("failed to list claims: %v", err)
		if apierrors.IsNotFound(err) {
			http.Error(w, "claim not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to load claim", http.StatusInternalServerError)
		return
	}
	if len(claimList.Items) == 0 {
		http.Error(w, "claim not found", http.StatusNotFound)
		return
	}
	for _, claim := range claimList.Items {
		if claim.Labels[controller.ManagedByLabelKey] != controller.ManagedByLabelValue {
			http.Error(w, "claim not managed by controller", http.StatusForbidden)
			return
		}

		if err := s.client.Delete(ctx, claim.DeepCopy()); err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "claim not found", http.StatusNotFound)
				return
			}
			http.Error(w, "failed to delete claim", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
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
