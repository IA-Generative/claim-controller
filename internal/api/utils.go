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
	"strings"
	"time"
	"k8s.io/client-go/util/retry"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
