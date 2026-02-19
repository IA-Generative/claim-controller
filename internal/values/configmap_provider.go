package values

import (
	"context"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
)

type ConfigMapProvider struct {
	kubeClient kubernetes.Interface
	namespace  string
	name       string
	key        string

	informer toolscache.SharedIndexInformer

	mu   sync.RWMutex
	data []byte
	ref  *metav1.OwnerReference
}

func NewConfigMapProvider(kubeClient kubernetes.Interface, namespace, name, key string) (*ConfigMapProvider, error) {
	if kubeClient == nil {
		return nil, fmt.Errorf("kube client is required")
	}
	if namespace == "" || name == "" || key == "" {
		return nil, fmt.Errorf("namespace, name and key are required")
	}

	provider := &ConfigMapProvider{
		kubeClient: kubeClient,
		namespace:  namespace,
		name:       name,
		key:        key,
	}

	if err := provider.loadInitial(context.Background()); err != nil {
		return nil, err
	}

	provider.setupInformer()
	return provider, nil
}

func (p *ConfigMapProvider) Start(ctx context.Context) error {
	if p.informer == nil {
		return nil
	}

	go p.informer.Run(ctx.Done())
	if !toolscache.WaitForCacheSync(ctx.Done(), p.informer.HasSynced) {
		return fmt.Errorf("configmap informer cache sync failed")
	}
	return nil
}

func (p *ConfigMapProvider) GetValues() ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.data) == 0 {
		return nil, fmt.Errorf("no values available in configmap %s/%s key %s", p.namespace, p.name, p.key)
	}
	return append([]byte(nil), p.data...), nil
}

func (p *ConfigMapProvider) GetOwnerReference() *metav1.OwnerReference {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.ref == nil {
		return nil
	}
	refCopy := *p.ref
	return &refCopy
}

func (p *ConfigMapProvider) Description() string {
	return fmt.Sprintf("configmap:%s/%s#%s", p.namespace, p.name, p.key)
}

func (p *ConfigMapProvider) loadInitial(ctx context.Context) error {
	cm, err := p.kubeClient.CoreV1().ConfigMaps(p.namespace).Get(ctx, p.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get configmap %s/%s: %w", p.namespace, p.name, err)
	}

	value, ok := cm.Data[p.key]
	if !ok || strings.TrimSpace(value) == "" {
		return fmt.Errorf("configmap %s/%s key %q not found or empty", p.namespace, p.name, p.key)
	}

	p.setDataAndRef([]byte(value), ownerReferenceFromConfigMap(cm))
	return nil
}

func (p *ConfigMapProvider) setupInformer() {
	factory := k8sinformers.NewSharedInformerFactoryWithOptions(
		p.kubeClient,
		0,
		k8sinformers.WithNamespace(p.namespace),
		k8sinformers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", p.name).String()
		}),
	)

	p.informer = factory.Core().V1().ConfigMaps().Informer()
	p.informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			p.updateFromObject(obj)
		},
		UpdateFunc: func(_, newObj any) {
			p.updateFromObject(newObj)
		},
		DeleteFunc: func(_ any) {
			p.setDataAndRef(nil, nil)
		},
	})
}

func (p *ConfigMapProvider) updateFromObject(obj any) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return
	}

	value, ok := cm.Data[p.key]
	if !ok || strings.TrimSpace(value) == "" {
		p.setDataAndRef(nil, nil)
		return
	}

	p.setDataAndRef([]byte(value), ownerReferenceFromConfigMap(cm))
}

func (p *ConfigMapProvider) setDataAndRef(data []byte, ref *metav1.OwnerReference) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if data == nil {
		p.data = nil
		p.ref = nil
		return
	}
	p.data = append([]byte(nil), data...)
	if ref == nil {
		p.ref = nil
		return
	}
	refCopy := *ref
	p.ref = &refCopy
}

func ownerReferenceFromConfigMap(cm *corev1.ConfigMap) *metav1.OwnerReference {
	if cm == nil {
		return nil
	}
	return &metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       cm.Name,
		UID:        cm.UID,
		BlockOwnerDeletion: func() *bool {
			b := true
			return &b
		}(),
	}
}
