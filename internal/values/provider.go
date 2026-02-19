package values

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Provider interface {
	Start(ctx context.Context) error
	GetValues() ([]byte, error)
	GetOwnerReference() *metav1.OwnerReference
	Description() string
}
