package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	activeClaimsGauge = promauto.With(metrics.Registry).NewGauge(prometheus.GaugeOpts{
		Name: "claim_controller_active_claims",
		Help: "Number of managed claims currently present.",
	})
	activeResourcesGauge = promauto.With(metrics.Registry).NewGauge(prometheus.GaugeOpts{
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

type resourceReadiness struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Ready     bool   `json:"ready"`
	Message   string `json:"message"`
}

func (r *ClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}

	if err := r.cleanupExpiredClaims(ctx); err != nil {
		return ctrl.Result{}, err
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

	isPreProvisioned := isPreProvisionedClaim(claim)

	expiresAt, err := time.Parse(time.RFC3339, claim.Annotations[ExpiresAtAnnotationKey])
	if err != nil {
		expiresAt = time.Now().UTC().Add(r.DefaultTTL)
	}

	if !isPreProvisioned && time.Now().UTC().After(expiresAt) {
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

	allReady, summary, resourcesStatus, err := r.evaluateClaimReadiness(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateClaimReadinessStatus(ctx, claim, allReady, summary, resourcesStatus); err != nil {
		return ctrl.Result{}, err
	}

	_ = r.refreshMetrics(ctx)
	nextCheck := time.Until(expiresAt)
	if isPreProvisioned {
		nextCheck = r.ReconcileInterval
	}
	if nextCheck < 5*time.Second {
		nextCheck = 5 * time.Second
	}
	if !allReady && nextCheck > 3*time.Second {
		nextCheck = 3 * time.Second
	}
	if r.ReconcileInterval > 0 && r.ReconcileInterval < nextCheck {
		nextCheck = r.ReconcileInterval
	}

	return ctrl.Result{RequeueAfter: nextCheck}, nil
}

func (r *ClaimReconciler) evaluateClaimReadiness(ctx context.Context, claim *corev1.ConfigMap) (bool, string, []resourceReadiness, error) {
	resources, err := templatesFromClaim(claim)
	if err != nil {
		return false, "", nil, err
	}

	isPreProvisioned := isPreProvisionedClaim(claim)

	allReady := true
	readyCount := 0
	statuses := make([]resourceReadiness, 0, len(resources))

	for _, resourceTemplate := range resources {
		if isPreProvisioned && isLazyProvisionedResource(resourceTemplate) {
			continue
		}

		resourceObj := &unstructured.Unstructured{}
		resourceObj.SetGroupVersionKind(resourceTemplate.GroupVersionKind())
		resourceObj.SetName(resourceTemplate.GetName())

		isNamespaced, err := r.isNamespacedResource(resourceObj)
		if err != nil {
			return false, "", nil, fmt.Errorf("resolve resource scope for %s %s: %w", resourceObj.GetKind(), resourceObj.GetName(), err)
		}
		if isNamespaced {
			resourceObj.SetNamespace(claim.Namespace)
		}

		if err := r.Get(ctx, client.ObjectKeyFromObject(resourceObj), resourceObj); err != nil {
			if apierrors.IsNotFound(err) {
				allReady = false
				statuses = append(statuses, resourceReadiness{
					Kind:      resourceTemplate.GetKind(),
					Name:      resourceTemplate.GetName(),
					Namespace: resourceObj.GetNamespace(),
					Ready:     false,
					Message:   "not created yet",
				})
				continue
			}
			return false, "", nil, err
		}

		ready, message := assessResourceReadiness(resourceObj)
		if ready {
			readyCount++
		} else {
			allReady = false
		}

		statuses = append(statuses, resourceReadiness{
			Kind:      resourceObj.GetKind(),
			Name:      resourceObj.GetName(),
			Namespace: resourceObj.GetNamespace(),
			Ready:     ready,
			Message:   message,
		})
	}

	summary := fmt.Sprintf("%d/%d resources ready", readyCount, len(resources))
	if allReady {
		summary = "all resources ready"
	}

	return allReady, summary, statuses, nil
}

func (r *ClaimReconciler) updateClaimReadinessStatus(ctx context.Context, claim *corev1.ConfigMap, allReady bool, summary string, resources []resourceReadiness) error {
	resourcesJSON, err := json.Marshal(resources)
	if err != nil {
		return err
	}

	statusValue := "pending"
	if allReady {
		statusValue = "ready"
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(claim), current); err != nil {
			return client.IgnoreNotFound(err)
		}

		if current.Data == nil {
			current.Data = map[string]string{}
		}

		if current.Data[ClaimStatusDataKey] == statusValue &&
			current.Data[ClaimStatusMessageDataKey] == summary &&
			current.Data[ClaimResourcesStatusDataKey] == string(resourcesJSON) {
			return nil
		}

		current.Data[ClaimStatusDataKey] = statusValue
		current.Data[ClaimStatusMessageDataKey] = summary
		current.Data[ClaimResourcesStatusDataKey] = string(resourcesJSON)

		return r.Update(ctx, current)
	})
}

func assessResourceReadiness(obj *unstructured.Unstructured) (bool, string) {
	switch strings.ToLower(obj.GetKind()) {
	case "pod":
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if phase == "Succeeded" {
			return true, "pod succeeded"
		}
		if phase == "Failed" {
			return false, "pod failed"
		}
		ready, readyFound := conditionStatus(obj.Object, "status", "conditions", "Ready")
		if phase == "Running" && readyFound && ready {
			return true, "pod ready"
		}
		return false, fmt.Sprintf("pod phase=%s", phase)
	case "deployment":
		desiredReplicas, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if desiredReplicas == 0 {
			desiredReplicas = 1
		}
		readyReplicas, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
		available, availableFound := conditionStatus(obj.Object, "status", "conditions", "Available")
		if readyReplicas >= desiredReplicas && (!availableFound || available) {
			return true, fmt.Sprintf("deployment ready (%d/%d)", readyReplicas, desiredReplicas)
		}
		return false, fmt.Sprintf("deployment not ready (%d/%d)", readyReplicas, desiredReplicas)
	default:
		return true, "resource exists"
	}
}

func conditionStatus(object map[string]any, section string, field string, conditionType string) (bool, bool) {
	conditions, found, _ := unstructured.NestedSlice(object, section, field)
	if !found {
		return false, false
	}
	for _, rawCondition := range conditions {
		conditionMap, ok := rawCondition.(map[string]any)
		if !ok {
			continue
		}
		conditionName, _, _ := unstructured.NestedString(conditionMap, "type")
		if !strings.EqualFold(conditionName, conditionType) {
			continue
		}
		statusText, _, _ := unstructured.NestedString(conditionMap, "status")
		statusBool, err := strconv.ParseBool(strings.ToLower(statusText))
		if err != nil {
			return false, true
		}
		return statusBool, true
	}
	return false, false
}

func (r *ClaimReconciler) cleanupExpiredClaims(ctx context.Context) error {
	claims := &corev1.ConfigMapList{}
	if err := r.List(ctx, claims, client.InNamespace(r.Namespace), client.MatchingLabels{ManagedByLabelKey: ManagedByLabelValue}); err != nil {
		return err
	}

	now := time.Now().UTC()
	for i := range claims.Items {
		claim := &claims.Items[i]
		if isPreProvisionedClaim(claim) {
			continue
		}

		expiresAt, err := time.Parse(time.RFC3339, claim.Annotations[ExpiresAtAnnotationKey])
		if err != nil || now.Before(expiresAt) {
			continue
		}

		if err := r.cleanupClaimResources(ctx, claim); err != nil {
			return err
		}
		if err := r.Delete(ctx, claim); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	return nil
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

	isPreProvisioned := isPreProvisionedClaim(claim)

	for _, resourceTemplate := range resources {
		if isPreProvisioned && isLazyProvisionedResource(resourceTemplate) {
			continue
		}

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

		if isPreProvisionedClaim(&claim) {
			for _, template := range templates {
				if isLazyProvisionedResource(template) {
					continue
				}
				resources++
			}
			continue
		}

		resources += len(templates)
	}
	activeResourcesGauge.Set(float64(resources))

	return nil
}

func isPreProvisionedClaim(claim *corev1.ConfigMap) bool {
	if claim == nil {
		return false
	}

	value := strings.TrimSpace(claim.Annotations[PreProvisionedAnnotationKey])
	return strings.EqualFold(value, "true")
}

func isLazyProvisionedResource(resource *unstructured.Unstructured) bool {
	if resource == nil {
		return false
	}

	value := strings.TrimSpace(resource.GetAnnotations()[LazyProvisioningAnnotationKey])
	return strings.EqualFold(value, "true")
}
