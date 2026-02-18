# claim-controller

Namespaced Kubernetes operator + HTTP API for claim-based ephemeral workloads.

## Behavior

- `POST /claim` takes no parameters.
- The API creates a managed claim object (`ConfigMap`) with random Pod/Service names.
- Controller reconciles claims and creates a Pod + Service from a Helm-style template file + separate `values.yaml` loaded at startup.
- API returns the generated service FQDN: `<service>.<namespace>.svc.cluster.local`.
- Claims expire after TTL (default `10m`), and controller deletes claim resources.
- Metrics are exposed on controller-runtime metrics endpoint (`/metrics`) and include:
  - `claim_controller_active_claims`
  - `claim_controller_active_pods`
  - `claim_controller_active_services`

## Run locally

Requires access to a Kubernetes cluster (for in-cluster or local kubeconfig auth).

```bash
go run ./cmd/server \
  --namespace=default \
  --template-path=config/template/resources.yaml \
  --values-path=config/template/values.yaml \
  --api-addr=:8080 \
  --metrics-addr=:8081
```

## Config file support

You can pass a config file through `--config` (YAML or JSON).


```bash
go run ./cmd/server
```

CLI flags still override values loaded from the config file.

Configuration precedence is:

1. CLI flags
2. Environment variables
3. Config file (`--config` / `CONFIG_PATH`)
4. Built-in defaults

Supported environment variables (with defaults):

- `CONFIG_PATH` (default: empty)
- `NAMESPACE` (default: `default`)
- `TEMPLATE_PATH` (default: `config/template/resources.yaml`)
- `VALUES_PATH` (default: `config/template/values.yaml`)
- `API_ADDR` (default: `:8080`)
- `METRICS_ADDR` (default: `:8081`)
- `PROBE_ADDR` (default: `:8082`)
- `DEFAULT_TTL` (default: `10m`)
- `RECONCILE_INTERVAL` (default: `30s`)

## Hot reload with Air

Install Air and run:

```bash
air
```

The project includes `.air.toml` configured for `cmd/server`.

Create a claim:

```bash
curl -X POST http://localhost:8080/claim
```

## Deploy (namespaced, non-cluster-wide)

```bash
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/samples/template-configmap.yaml
kubectl apply -f config/manager/deployment.yaml
```

The provided RBAC uses `Role`/`RoleBinding` in one namespace only (no CRD, no cluster-wide permissions).
Deployment manifests mount two volumes: one for Helm template (`resources.yaml`) and one for runtime values (`values.yaml`).

## Docker

Build image:

```bash
docker build -t nonot/claim-controller:dev .
```

Run with local kubeconfig mounted:

```bash
docker compose up --build
```

`docker compose` uses the `dev` Docker target and runs `air` for hot reload.

## Helm chart

Chart location: `charts/claim-controller`

Install in a namespace:

```bash
helm upgrade --install claim-controller ./charts/claim-controller -n default --create-namespace
```

Override image for your build:

```bash
helm upgrade --install claim-controller ./charts/claim-controller \
  -n default \
  --set image.repository=nonot/claim-controller \
  --set image.tag=dev
```