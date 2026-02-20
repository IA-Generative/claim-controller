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
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/util/retry"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/template"
)

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

func claimLifetimeRatio(claim corev1.ConfigMap) (float64, bool) {
	now := time.Now().UTC()

	totalActualSeconds, ok := claimTotalActualDurationSeconds(claim, now)
	if !ok {
		return 0, false
	}

	expectedLifetimeSeconds, ok := claimExpectedLifetimeSeconds(claim)
	if !ok || expectedLifetimeSeconds <= 0 {
		return 0, false
	}

	return totalActualSeconds / expectedLifetimeSeconds, true
}

func claimUsageRatio(claim corev1.ConfigMap) (float64, bool) {
	now := time.Now().UTC()

	usageActualSeconds, ok := claimUsageActualDurationSeconds(claim, now)
	if !ok {
		return 0, false
	}

	usageExpectedSeconds, ok := claimUsageExpectedDurationSeconds(claim)
	if !ok || usageExpectedSeconds <= 0 {
		return 0, false
	}

	return usageActualSeconds / usageExpectedSeconds, true
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

func claimClaimedAtTime(claim corev1.ConfigMap) (time.Time, bool) {
	if !claim.CreationTimestamp.IsZero() {
		claimedAtRaw := strings.TrimSpace(claim.Annotations[controller.ClaimedAtAnnotationKey])
		if claimedAtRaw != "" {
			if claimedAt, err := time.Parse(time.RFC3339, claimedAtRaw); err == nil {
				return claimedAt.UTC(), true
			}
		}
		return claim.CreationTimestamp.Time.UTC(), true
	}

	return time.Time{}, false
}

func claimTotalActualDurationSeconds(claim corev1.ConfigMap, now time.Time) (float64, bool) {
	if claim.CreationTimestamp.IsZero() {
		return 0, false
	}

	total := now.Sub(claim.CreationTimestamp.Time)
	if total < 0 {
		total = 0
	}

	return total.Seconds(), true
}

func claimIdleDurationSeconds(claim corev1.ConfigMap) (float64, bool) {
	if claim.CreationTimestamp.IsZero() {
		return 0, false
	}

	claimedAt, ok := claimClaimedAtTime(claim)
	if !ok {
		return 0, false
	}

	idle := claimedAt.Sub(claim.CreationTimestamp.Time)
	if idle < 0 {
		idle = 0
	}

	return idle.Seconds(), true
}

func claimUsageActualDurationSeconds(claim corev1.ConfigMap, now time.Time) (float64, bool) {
	claimedAt, ok := claimClaimedAtTime(claim)
	if !ok {
		return 0, false
	}

	usage := now.Sub(claimedAt)
	if usage < 0 {
		usage = 0
	}

	return usage.Seconds(), true
}

func claimUsageExpectedDurationSeconds(claim corev1.ConfigMap) (float64, bool) {
	claimedAt, ok := claimClaimedAtTime(claim)
	if !ok {
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

	usageExpected := expiresAt.Sub(claimedAt)
	if usageExpected <= 0 {
		return 0, false
	}

	return usageExpected.Seconds(), true
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

var rnd = rand.New(rand.NewSource(time.Now().UnixNano()))

func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rnd.Intn(len(letters))]
	}
	return strings.ToLower(string(b))
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
		return 0, fmt.Errorf("invalid ttl duration: %w", err)
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

func (s *Server) renewClaim(ctx context.Context, claim corev1.ConfigMap, ttl time.Duration) (*corev1.ConfigMap, error) {
	now := time.Now().UTC()
	claimedAt := claim.CreationTimestamp.Time.UTC()
	if claimedAtRaw := strings.TrimSpace(claim.Annotations[controller.ClaimedAtAnnotationKey]); claimedAtRaw != "" {
		if parsedClaimedAt, err := time.Parse(time.RFC3339, claimedAtRaw); err == nil {
			claimedAt = parsedClaimedAt.UTC()
		}
	}

	maxExpiresAt := claimedAt.Add(s.maxTTL)
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

func (s *Server) ensurePreProvisionedClaims(ctx context.Context) error {
	if s.preProvisionCount <= 0 {
		return nil
	}

	claimList := &corev1.ConfigMapList{}
	if err := s.client.List(ctx, claimList, client.InNamespace(s.namespace), client.MatchingLabels{controller.ManagedByLabelKey: controller.ManagedByLabelValue}); err != nil {
		return err
	}

	currentCount := 0
	for i := range claimList.Items {
		claim := &claimList.Items[i]
		if !strings.EqualFold(strings.TrimSpace(claim.Annotations[controller.PreProvisionedAnnotationKey]), "true") {
			continue
		}

		currentCount++
	}

	missing := s.preProvisionCount - currentCount
	for i := 0; i < missing; i++ {
		claimID := randomSuffix(8)
		expiresAt := time.Now().UTC().Add(s.maxTTL)
		if _, err := s.createClaim(ctx, claimID, expiresAt, true); err != nil {
			return err
		}
		claimsPreProvisionedCreatedTotal.Inc()
	}

	return nil
}

func (s *Server) acquireClaim(ctx context.Context, ttl time.Duration) (*corev1.ConfigMap, string, time.Time, bool, error) {
	claim, err := s.acquirePreProvisionedClaim(ctx, ttl)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}
	if claim != nil {
		claimID := strings.TrimSpace(claim.Labels[controller.ClaimLabelKeyId])
		expiresAt, _ := time.Parse(time.RFC3339, claim.Annotations[controller.ExpiresAtAnnotationKey])
		claimsReusedPreProvisionedTotal.Inc()
		return claim, claimID, expiresAt, true, nil
	}

	claimID := randomSuffix(8)
	expiresAt := time.Now().UTC().Add(ttl)
	created, err := s.createClaim(ctx, claimID, expiresAt, false)
	if err != nil {
		return nil, "", time.Time{}, false, err
	}

	claimsCreatedTotal.Inc()
	claimsCreatedOnDemandTotal.Inc()
	return created, claimID, expiresAt, false, nil
}

func (s *Server) acquirePreProvisionedClaim(ctx context.Context, ttl time.Duration) (*corev1.ConfigMap, error) {
	if s.preProvisionCount <= 0 {
		return nil, nil
	}

	claimList := &corev1.ConfigMapList{}
	if err := s.client.List(ctx, claimList, client.InNamespace(s.namespace), client.MatchingLabels{controller.ManagedByLabelKey: controller.ManagedByLabelValue}); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	for i := range claimList.Items {
		candidate := claimList.Items[i]
		if !strings.EqualFold(strings.TrimSpace(candidate.Annotations[controller.PreProvisionedAnnotationKey]), "true") {
			continue
		}

		updated := candidate.DeepCopy()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &corev1.ConfigMap{}
			if err := s.client.Get(ctx, client.ObjectKeyFromObject(updated), current); err != nil {
				return err
			}

			if !strings.EqualFold(strings.TrimSpace(current.Annotations[controller.PreProvisionedAnnotationKey]), "true") {
				return apierrors.NewConflict(corev1.Resource("configmaps"), current.Name, errors.New("already claimed"))
			}

			claimedAt := now
			if claimedAtRaw := strings.TrimSpace(current.Annotations[controller.ClaimedAtAnnotationKey]); claimedAtRaw != "" {
				if parsedClaimedAt, err := time.Parse(time.RFC3339, claimedAtRaw); err == nil {
					claimedAt = parsedClaimedAt.UTC()
				}
			}
			maxExpiresAt := claimedAt.Add(s.maxTTL)
			if !maxExpiresAt.After(now) {
				return apierrors.NewConflict(corev1.Resource("configmaps"), current.Name, errors.New("pre-provisioned claim too old"))
			}

			expiresAt := now.Add(ttl)
			if expiresAt.After(maxExpiresAt) {
				expiresAt = maxExpiresAt
			}

			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[controller.PreProvisionedAnnotationKey] = "false"
			current.Annotations[controller.ClaimedAtAnnotationKey] = now.Format(time.RFC3339)
			current.Annotations[controller.ExpiresAtAnnotationKey] = expiresAt.Format(time.RFC3339)
			return s.client.Update(ctx, current)
		})
		if err != nil {
			continue
		}

		fresh := &corev1.ConfigMap{}
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: updated.Namespace, Name: updated.Name}, fresh); err != nil {
			return nil, err
		}

		claimsCreatedTotal.Inc()
		return fresh, nil
	}

	return nil, nil
}

func (s *Server) createClaim(ctx context.Context, claimID string, expiresAt time.Time, preProvisioned bool) (*corev1.ConfigMap, error) {
	claimName := fmt.Sprintf("claim-%s", claimID)
	claimedAt := ""
	if !preProvisioned {
		claimedAt = time.Now().UTC().Format(time.RFC3339)
	}

	resourceTemplate, err := s.loadResourceTemplate(claimID)
	if err != nil {
		return nil, err
	}
	if len(resourceTemplate.RenderedObjects) == 0 {
		return nil, fmt.Errorf("rendered templates must include at least one resource")
	}

	renderedResourcesBytes, err := json.Marshal(resourceTemplate.RenderedObjects)
	if err != nil {
		return nil, err
	}

	returnValuesBytes, err := json.Marshal(resourceTemplate.ReturnValues)
	if err != nil {
		return nil, err
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
				controller.ExpiresAtAnnotationKey:      expiresAt.Format(time.RFC3339),
				controller.CreatedByAnnotationKey:      controller.CreatedByAnnotationValue,
				controller.PreProvisionedAnnotationKey: strconv.FormatBool(preProvisioned),
			},
		},
		Data: map[string]string{
			controller.RenderedResourcesDataKey:    string(renderedResourcesBytes),
			controller.ReturnValuesDataKey:         string(returnValuesBytes),
			controller.ClaimStatusDataKey:          "pending",
			controller.ClaimStatusMessageDataKey:   "waiting for resources to be created",
			controller.ClaimResourcesStatusDataKey: "[]",
		},
	}

	if ownerRef := s.valuesProvider.GetOwnerReference(); ownerRef != nil {
		claim.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	}
	if claimedAt != "" {
		claim.Annotations[controller.ClaimedAtAnnotationKey] = claimedAt
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return s.client.Create(ctx, claim)
	})
	if err != nil {
		return nil, err
	}

	return claim, nil
}
