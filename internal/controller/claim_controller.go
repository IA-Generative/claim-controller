package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	activeClaimsGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "claim_controller_active_claims",
		Help: "Number of managed claims currently present.",
	})
	activeResourcesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "claim_controller_active_resources",
		Help: "Number of managed resources currently present.",
	})
)

type ClaimReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Namespace         string
	DefaultTTL        time.Duration
	ReconcileInterval time.Duration
	Recorder          record.EventRecorder
}

func (r *ClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}

	claim := &corev1.ConfigMap{}
	err := r.Get(ctx, req.NamespacedName, claim)
	if err != nil {
		if apierrors.IsNotFound(err) {
			_ = r.refreshMetrics(ctx)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if claim.Labels[ManagedByLabelKey] != ManagedByLabelValue {
		return ctrl.Result{}, nil
	}

	expiresAt, err := time.Parse(time.RFC3339, claim.Annotations[ExpiresAtAnnotationKey])
	if err != nil {
		expiresAt = time.Now().UTC().Add(r.DefaultTTL)
	}

	if time.Now().UTC().After(expiresAt) {
		if err := r.cleanupClaimResources(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		_ = r.refreshMetrics(ctx)
		return ctrl.Result{}, nil
	}

	if err := r.ensureClaimResources(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	_ = r.refreshMetrics(ctx)
	nextCheck := time.Until(expiresAt)
	if nextCheck < 5*time.Second {
		nextCheck = 5 * time.Second
	}
	if r.ReconcileInterval > 0 && r.ReconcileInterval < nextCheck {
		nextCheck = r.ReconcileInterval
	}

	return ctrl.Result{RequeueAfter: nextCheck}, nil
}

func (r *ClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Complete(r)
}

func (r *ClaimReconciler) ensureClaimResources(ctx context.Context, claim *corev1.ConfigMap) error {
	claimName := claim.Name
	resources, err := templatesFromClaim(claim)
	if err != nil {
		return err
	}

	for _, resourceTemplate := range resources {
		resourceObj := resourceTemplate.DeepCopy()
		isNamespaced, err := r.isNamespacedResource(resourceObj)
		if err != nil {
			return fmt.Errorf("resolve resource scope for %s %s: %w", resourceObj.GetKind(), resourceObj.GetName(), err)
		}

		lookupKey := client.ObjectKey{Name: resourceObj.GetName()}
		if isNamespaced {
			resourceObj.SetNamespace(claim.Namespace)
			lookupKey.Namespace = claim.Namespace
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(resourceObj.GroupVersionKind())
		err = r.Get(ctx, lookupKey, existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		labels := resourceObj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ManagedByLabelKey] = ManagedByLabelValue
		labels[ClaimLabelKey] = claimName
		resourceObj.SetLabels(labels)

		if err := ctrl.SetControllerReference(claim, resourceObj, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, resourceObj); err != nil {
			return err
		}
		r.Recorder.Eventf(claim, corev1.EventTypeNormal, "CreatedResource", "Created %s %s", resourceObj.GetKind(), resourceObj.GetName())
	}

	return nil
}

func templatesFromClaim(claim *corev1.ConfigMap) ([]*unstructured.Unstructured, error) {
	if claim.Data == nil {
		return nil, fmt.Errorf("claim missing rendered templates")
	}

	renderedResourcesRaw := claim.Data[RenderedResourcesDataKey]
	if renderedResourcesRaw == "" {
		return nil, fmt.Errorf("claim missing %s", RenderedResourcesDataKey)
	}

	var rawResources []map[string]any
	if err := json.Unmarshal([]byte(renderedResourcesRaw), &rawResources); err != nil {
		return nil, fmt.Errorf("decode rendered resources from claim: %w", err)
	}
	if len(rawResources) == 0 {
		return nil, fmt.Errorf("claim rendered resources must contain at least one resource")
	}

	resources := make([]*unstructured.Unstructured, 0, len(rawResources))
	for _, rawResource := range rawResources {
		resource := &unstructured.Unstructured{Object: rawResource}
		if resource.GetAPIVersion() == "" {
			return nil, fmt.Errorf("rendered resource missing apiVersion")
		}
		if resource.GetKind() == "" {
			return nil, fmt.Errorf("rendered resource missing kind")
		}
		if resource.GetName() == "" {
			return nil, fmt.Errorf("rendered resource missing metadata.name")
		}
		resources = append(resources, resource)
	}

	return resources, nil
}

func (r *ClaimReconciler) cleanupClaimResources(ctx context.Context, claim *corev1.ConfigMap) error {
	resources, err := templatesFromClaim(claim)
	if err != nil {
		return err
	}

	for _, resourceTemplate := range resources {
		resourceObj := &unstructured.Unstructured{}
		resourceObj.SetGroupVersionKind(resourceTemplate.GroupVersionKind())
		resourceObj.SetName(resourceTemplate.GetName())

		isNamespaced, err := r.isNamespacedResource(resourceObj)
		if err != nil {
			return fmt.Errorf("resolve resource scope for %s %s: %w", resourceObj.GetKind(), resourceObj.GetName(), err)
		}
		if isNamespaced {
			resourceObj.SetNamespace(claim.Namespace)
		}

		if err := r.Delete(ctx, resourceObj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	r.Recorder.Event(claim, corev1.EventTypeNormal, "Expired", "Claim expired and resources were deleted")
	return nil
}

func (r *ClaimReconciler) isNamespacedResource(obj *unstructured.Unstructured) (bool, error) {
	mapping, err := r.RESTMapper().RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
	if err != nil {
		return false, err
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func (r *ClaimReconciler) refreshMetrics(ctx context.Context) error {
	claims := &corev1.ConfigMapList{}
	if err := r.List(ctx, claims, client.InNamespace(r.Namespace), client.MatchingLabels{ManagedByLabelKey: ManagedByLabelValue}); err != nil {
		return err
	}
	activeClaimsGauge.Set(float64(len(claims.Items)))

	resources := 0
	for _, claim := range claims.Items {
		templates, err := templatesFromClaim(&claim)
		if err != nil {
			continue
		}
		resources += len(templates)
	}
	activeResourcesGauge.Set(float64(resources))

	return nil
}
