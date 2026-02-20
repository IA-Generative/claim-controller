package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nonot/claim-controller/internal/api"
	"github.com/nonot/claim-controller/internal/config"
	"github.com/nonot/claim-controller/internal/controller"
	"github.com/nonot/claim-controller/internal/values"
)

func main() {
	var (
		configPath          string
		namespace           string
		templatePath        string
		valuesPath          string
		valuesConfigMapName string
		valuesConfigMapKey  string
		apiAddr             string
		metricsAddr         string
		defaultTTL          time.Duration
		maxTTL              time.Duration
		reconcileInterval   time.Duration
		probeAddr           string
		controllerLogLevel  int
	)

	const (
		defaultNamespace         = "default"
		defaultTemplatePath      = "config/template/resources.yaml"
		defaultValuesPath        = "/values/values.yaml"
		defaultAPIAddr           = "0.0.0.0:8080"
		defaultMetricsAddr       = "0.0.0.0:8081"
		defaultProbeAddr         = "0.0.0.0:8082"
		defaultTTLValue          = 3 * time.Minute
		defaultMaxTTLValue       = 10 * time.Minute
		defaultReconcileInterval = 30 * time.Second
	)

	bootstrap := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	bootstrap.StringVar(&configPath, "config", getEnv("CONFIG_PATH", ""), "path to YAML/JSON config file")
	_ = bootstrap.Parse(os.Args[1:])

	namespaceDefault := resolveString("NAMESPACE", os.Getenv("NAMESPACE"), defaultNamespace)
	valuesPathDefault := resolveString("VALUES_PATH", os.Getenv("VALUES_PATH"), defaultValuesPath)
	valuesConfigMapNameDefault := resolveString("VALUES_CONFIGMAP_NAME", os.Getenv("VALUES_CONFIGMAP_NAME"), "")
	valuesConfigMapKeyDefault := resolveString("VALUES_CONFIGMAP_KEY", os.Getenv("VALUES_CONFIGMAP_KEY"), "")
	templatePathDefault := resolveString("TEMPLATE_PATH", os.Getenv("TEMPLATE_PATH"), defaultTemplatePath)
	apiAddrDefault := resolveString("API_ADDR", os.Getenv("API_ADDR"), defaultAPIAddr)
	metricsAddrDefault := resolveString("METRICS_ADDR", os.Getenv("METRICS_ADDR"), defaultMetricsAddr)
	probeAddrDefault := resolveString("PROBE_ADDR", os.Getenv("PROBE_ADDR"), defaultProbeAddr)
	defaultTTLDefault := resolveDuration("DEFAULT_TTL", os.Getenv("DEFAULT_TTL"), defaultTTLValue)
	maxTTLDefault := resolveDuration("MAX_TTL", os.Getenv("MAX_TTL"), defaultMaxTTLValue)
	reconcileIntervalDefault := resolveDuration("RECONCILE_INTERVAL", os.Getenv("RECONCILE_INTERVAL"), defaultReconcileInterval)

	flag.StringVar(&namespace, "namespace", namespaceDefault, "namespace watched and managed by the controller")
	flag.StringVar(&valuesPath, "values-path", valuesPathDefault, "path to Helm values file")
	flag.StringVar(&valuesConfigMapName, "values-configmap-name", valuesConfigMapNameDefault, "ConfigMap name containing values template")
	flag.StringVar(&valuesConfigMapKey, "values-configmap-key", valuesConfigMapKeyDefault, "ConfigMap data key containing values template")
	flag.StringVar(&templatePath, "template-path", templatePathDefault, "path to Helm template file")
	flag.StringVar(&apiAddr, "api-addr", apiAddrDefault, "claim API listen address")
	flag.StringVar(&metricsAddr, "metrics-addr", metricsAddrDefault, "metrics listen address")
	flag.StringVar(&probeAddr, "health-probe-addr", probeAddrDefault, "probe listen address")
	flag.DurationVar(&defaultTTL, "default-ttl", defaultTTLDefault, "default claim lifetime")
	flag.DurationVar(&maxTTL, "max-ttl", maxTTLDefault, "maximum claim lifetime")
	flag.DurationVar(&reconcileInterval, "reconcile-interval", reconcileIntervalDefault, "controller periodic reconcile interval")
	flag.IntVar(&controllerLogLevel, "zap-log-level", 0, "zap logger level")
	flag.Parse()

	if maxTTL < defaultTTL {
		maxTTL = defaultTTL
	}

	logLevel := zapcore.Level(-controllerLogLevel)
	ctrl.SetLogger(zap.New(
		zap.UseDevMode(false),
		zap.Level(logLevel),
	))

	logger := ctrl.Log.WithName("main")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	managedSelector := labels.SelectorFromSet(labels.Set{
		controller.ManagedByLabelKey: controller.ManagedByLabelValue,
	})

	restConfig := ctrl.GetConfigOrDie()

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		panic(fmt.Errorf("create kube client: %w", err))
	}

	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces:    map[string]cache.Config{namespace: {}},
			DefaultLabelSelector: managedSelector,
			DefaultTransform:     cache.TransformStripManagedFields(),
		},
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		panic(fmt.Errorf("create manager: %w", err))
	}

	reconciler := &controller.ClaimReconciler{
		Client:            manager.GetClient(),
		Scheme:            manager.GetScheme(),
		Namespace:         namespace,
		DefaultTTL:        defaultTTL,
		ReconcileInterval: reconcileInterval,
		Recorder:          manager.GetEventRecorderFor("claim-controller"),
	}

	if err := reconciler.SetupWithManager(manager); err != nil {
		panic(fmt.Errorf("setup reconciler: %w", err))
	}

	apiServer := api.NewServer(api.Config{
		Namespace:      namespace,
		DefaultTTL:     defaultTTL,
		MaxTTL:         maxTTL,
		TemplatePath:   templatePath,
		ValuesProvider: resolveValuesProvider(logger, kubeClient, namespace, valuesConfigMapName, valuesConfigMapKey, valuesPath),
		Client:         manager.GetClient(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := apiServer.Start(ctx); err != nil {
		panic(fmt.Errorf("start api server dependencies: %w", err))
	}

	httpServer := &http.Server{
		Addr:              apiAddr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "api server stopped")
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "failed to shutdown api server")
		}
	}()

	logger.Info("starting manager", "namespace", namespace, "apiAddr", apiAddr, "metricsAddr", metricsAddr, "templatePath", templatePath, "valuesPath", valuesPath, "defaultTTL", defaultTTL.String(), "maxTTL", maxTTL.String())
	if err := manager.Start(ctx); err != nil {
		panic(fmt.Errorf("run manager: %w", err))
	}
}

func resolveValuesProvider(logger logr.Logger, kubeClient kubernetes.Interface, namespace, configMapName, configMapKey, valuesPath string) values.Provider {
	if configMapKey != "" && configMapName != "" {
		configMapProvider, err := values.NewConfigMapProvider(kubeClient, namespace, configMapName, configMapKey)
		if err == nil {
			logger.Info("using configmap values provider", "source", configMapProvider.Description())
			return configMapProvider
		}
	}

	if valuesPath != "" {
		fileProvider, err := values.NewFileProvider(valuesPath)
		if err != nil {
		}
		logger.Info("using file values provider", "source", fileProvider.Description())
		return fileProvider
	}

	panic(fmt.Errorf("no valid values provider found, please provide either configmap or file values"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func resolveString(envName, fileValue, fallback string) string {
	return firstNonEmpty(os.Getenv(envName), fileValue, fallback)
}

func resolveDuration(envName, fileValue string, fallback time.Duration) time.Duration {
	if envValue := os.Getenv(envName); envValue != "" {
		if d, err := time.ParseDuration(envValue); err == nil {
			return d
		}
	}

	return config.ParseDurationOrFallback(fileValue, fallback)
}

func getEnv(name, fallback string) string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v
}
