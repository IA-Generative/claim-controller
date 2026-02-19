package values

import (
	"context"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type FileProvider struct {
	path string
	data []byte
}

func NewFileProvider(path string) (*FileProvider, error) {
	if path == "" {
		return nil, fmt.Errorf("values file path is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read values file %q: %w", path, err)
	}

	return &FileProvider{path: path, data: data}, nil
}

func (p *FileProvider) Start(context.Context) error {
	return nil
}

func (p *FileProvider) GetValues() ([]byte, error) {
	return append([]byte(nil), p.data...), nil
}

func (p *FileProvider) GetOwnerReference() *metav1.OwnerReference {
	return nil
}

func (p *FileProvider) Description() string {
	return "file:" + p.path
}
