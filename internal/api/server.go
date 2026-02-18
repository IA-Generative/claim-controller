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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/template"
)

type Config struct {
	Namespace           string
	DefaultTTL          time.Duration
	TemplatePath        string
	ValuesPath          string
	ValuesConfigMapName string
	ValuesConfigMapKey  string
	KubeClient          kubernetes.Interface
	Client              client.Client
}

type Server struct {
	namespace           string
	defaultTTL          time.Duration
	templatePath        string
	valuesPath          string
	valuesConfigMapName string
	valuesConfigMapKey  string
	kubeClient          kubernetes.Interface
	valuesInformer      toolscache.SharedIndexInformer
	valuesMu            sync.RWMutex
	valuesData          []byte
	client              client.Client
	mux                 *http.ServeMux
}

func NewServer(cfg Config) *Server {
	s := &Server{
		namespace:           cfg.Namespace,
		defaultTTL:          cfg.DefaultTTL,
		templatePath:        cfg.TemplatePath,
		valuesPath:          cfg.ValuesPath,
		valuesConfigMapName: cfg.ValuesConfigMapName,
		valuesConfigMapKey:  cfg.ValuesConfigMapKey,
		kubeClient:          cfg.KubeClient,
		client:              cfg.Client,
		mux:                 http.NewServeMux(),
	}
	s.setupValuesInformer()
	s.routes()
	return s
}

func (s *Server) Start(ctx context.Context) {
	if s.valuesInformer == nil {
		return
	}

	go s.valuesInformer.Run(ctx.Done())
	if !toolscache.WaitForCacheSync(ctx.Done(), s.valuesInformer.HasSynced) {
		log.Printf("values informer cache sync failed")
	}
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/claim", s.handleClaim)
	s.mux.HandleFunc("/release", s.handleRelease)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

type releaseRequest struct {
	ID string `json:"id"`
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
	resourceTemplate.ReturnValues["id"] = claimID
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

	writeJSON(w, http.StatusCreated, resourceTemplate.ReturnValues)
}

func (s *Server) loadResourceTemplate(claimID string) (template.ResourceTemplate, error) {
	if valuesData, ok := s.getValuesFromInformer(); ok {
		return template.LoadResourceTemplateFromValuesData(s.templatePath, valuesData, claimID)
	}

	return template.LoadResourceTemplate(s.templatePath, s.valuesPath, claimID)
}

func (s *Server) setupValuesInformer() {
	if s.kubeClient == nil || s.valuesConfigMapName == "" || s.valuesConfigMapKey == "" {
		return
	}

	factory := k8sinformers.NewSharedInformerFactoryWithOptions(
		s.kubeClient,
		0,
		k8sinformers.WithNamespace(s.namespace),
		k8sinformers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", s.valuesConfigMapName).String()
		}),
	)

	s.valuesInformer = factory.Core().V1().ConfigMaps().Informer()
	s.valuesInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			s.updateValuesFromConfigMap(obj)
		},
		UpdateFunc: func(_, newObj any) {
			s.updateValuesFromConfigMap(newObj)
		},
		DeleteFunc: func(_ any) {
			s.setValuesData(nil)
		},
	})
}

func (s *Server) updateValuesFromConfigMap(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return
	}

	value, ok := cm.Data[s.valuesConfigMapKey]
	if !ok || strings.TrimSpace(value) == "" {
		log.Printf("values configmap key %q not found or empty in %s/%s", s.valuesConfigMapKey, s.namespace, s.valuesConfigMapName)
		s.setValuesData(nil)
		return
	}

	s.setValuesData([]byte(value))
}

func (s *Server) setValuesData(data []byte) {
	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()
	if data == nil {
		s.valuesData = nil
		return
	}
	s.valuesData = append([]byte(nil), data...)
}

func (s *Server) getValuesFromInformer() ([]byte, bool) {
	s.valuesMu.RLock()
	defer s.valuesMu.RUnlock()
	if len(s.valuesData) == 0 {
		return nil, false
	}
	return append([]byte(nil), s.valuesData...), true
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req releaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	claimID := strings.TrimSpace(req.ID)
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
	writeJSON(w, http.StatusOK, map[string]string{"deleted": claimID})
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
