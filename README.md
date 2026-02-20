# claim-controller

Namespaced Kubernetes operator + HTTP API for claim-based ephemeral workloads.

## Behavior

- `POST /claim` accepts optional JSON body `{ "ttl": "<duration>" }`.
- `POST /renew/{id}` extends claim expiration with the same TTL rules.
- The API can pre-provision a pool of claims in advance (`PRE_PROVISION_CLAIMS_COUNT`).
- Resources annotated with `claim.controller/lazy.provisionning: "true"` are deferred until a pre-provisioned claim is actually used.
- The API creates a managed claim object (`ConfigMap`) with random Pod/Service names.
- Controller reconciles claims and creates a Pod + Service from a Helm-style template file + separate `values.yaml` loaded at startup.
- API returns the generated service FQDN: `<service>.<namespace>.svc.cluster.local`.
- Claims expire after TTL (default `10m`), client-provided TTL is capped by `maxTTL`, and controller deletes claim resources.
- Metrics are exposed on controller-runtime metrics endpoint (`/metrics`) and include:
  - `claim_controller_claims_created_total`: incremented for every successful `/claim` response. Scenario: client asks a claim and gets `201`.
  - `claim_controller_claims_created_ondemand_total`: incremented when no pre-provisioned claim is available and a fresh claim is created. Scenario: pool empty, API creates one immediately.
  - `claim_controller_claims_preprovisioned_created_total`: incremented when the background pool filler creates claims in advance. Scenario: pool target is 5 and current is 3, two creations happen.
  - `claim_controller_claims_reused_preprovisioned_total`: incremented when `/claim` reuses a pre-provisioned claim. Scenario: client request consumes one warm claim.
  - `claim_controller_claims_released_total`: incremented on successful release. Scenario: client calls `/release/{id}` and claim is deleted.
  - `claim_controller_claim_ready_duration_seconds`: histogram of wait time until claim resources are ready. Scenario: claim takes 8s before status becomes `ready`.
  - `claim_controller_claim_idle_duration_seconds`: histogram of idle time before effective claim usage (`creation` → `claimed-at`). Scenario: pre-provisioned claim waits 45s in pool before first use.
  - `claim_controller_claim_usage_duration_seconds`: histogram of real usage time (`claimed-at` → release). Scenario: claim is actively used for 2m30s.
  - `claim_controller_claim_lifetime_duration_seconds`: histogram of total real lifetime (`creation` → release). Scenario: claim lives 3m overall including idle + usage.
  - `claim_controller_claim_total_duration_seconds`: histogram of configured total lifetime (`creation` → `expires-at`). Scenario: claim configured with 10m total TTL window.
  - `claim_controller_claim_lifetime_expected_ratio`: histogram ratio `real lifetime / configured total lifetime`. Scenario: released halfway through TTL gives ratio close to `0.5`.
  - `claim_controller_claim_usage_expected_ratio`: histogram ratio `real usage / expected usage` where expected usage is (`expires-at` − `claimed-at`). Scenario: claimed at T+1m, released at T+4m on a 10m max window.
  - `claim_controller_timedout_claims_total`: incremented when API times out waiting for readiness. Scenario: resources never become ready within wait timeout.
  - `claim_controller_active_claims`: gauge of currently existing managed claims. Scenario: 7 active claims present now.
  - `claim_controller_active_resources`: gauge of currently existing managed resources derived from active claims. Scenario: each claim has pod+service, 7 claims show ~14 resources.

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
- `MAX_TTL` (default: `10m`)
- `RECONCILE_INTERVAL` (default: `30s`)
- `PRE_PROVISION_CLAIMS_COUNT` (default: `0`)

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

Create a claim with custom TTL:

```bash
curl -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d '{"ttl":"5m"}'
```

Renew a claim:

```bash
curl -X POST http://localhost:8080/renew/<claim-id> \
  -H 'Content-Type: application/json' \
  -d '{"ttl":"2m"}'
```

## Deploy (namespaced, non-cluster-wide)

```bash
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/manager/deployment.yaml
```

The provided RBAC uses `Role`/`RoleBinding` in one namespace only (no CRD, no cluster-wide permissions).
Deployment uses template files baked into the container image at `config/template/resources.yaml` and `config/template/values.yaml`.

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