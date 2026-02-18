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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/template"
)

type Config struct {
	Namespace    string
	DefaultTTL   time.Duration
	TemplatePath string
	ValuesPath   string
	Client       client.Client
}

type Server struct {
	namespace    string
	defaultTTL   time.Duration
	templatePath string
	valuesPath   string
	client       client.Client
	mux          *http.ServeMux
}

func NewServer(cfg Config) *Server {
	s := &Server{
		namespace:    cfg.Namespace,
		defaultTTL:   cfg.DefaultTTL,
		templatePath: cfg.TemplatePath,
		valuesPath:   cfg.ValuesPath,
		client:       cfg.Client,
		mux:          http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/claim", s.handleClaim)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
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

	resourceTemplate, err := template.LoadResourceTemplate(s.templatePath, s.valuesPath, claimID)
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

	// payload := map[string]any{
	// 	"claim":     claimName,
	// 	"expiresAt": expiresAt.Format(time.RFC3339),
	// 	"resources": resourceTemplate.Resources,
	// 	"returns":   resourceTemplate.ReturnValues,
	// }

	writeJSON(w, http.StatusCreated, resourceTemplate.ReturnValues)
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
