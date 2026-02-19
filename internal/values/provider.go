package values

import "context"

type Provider interface {
	Start(ctx context.Context) error
	GetValues() ([]byte, error)
	Description() string
}
