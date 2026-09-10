# TogetherGo on Kubernetes with Consul

This directory contains:

- `togethergo/` — the Helm chart for identity, trip, chat and the production web image;
- `consul/values.yaml` — values for the official HashiCorp Consul Helm chart;
- `consul/templates/` — namespaces, Consul API gateway routes and intentions;
- `values.example.yaml` — the ECR and environment overrides to copy locally.

The chart does not install PostgreSQL/PostGIS, RabbitMQ or Redis. Supply managed
endpoints for them. RabbitMQ must already contain the exchanges, queues and
bindings from `deploy/rabbitmq/definitions.json`, and the three service
databases must already exist with their own restricted users.

## Prerequisites

- an EKS cluster and working `kubectl` context (see `terraform/cluster/README.md`);
- Helm 3;
- the Kubernetes Gateway API CRDs;
- the EBS CSI driver and a `gp3` StorageClass;
- reachable PostgreSQL/PostGIS, RabbitMQ and Redis endpoints;
- four ECR images: `identity`, `trip`, `chat`, and the production `web` image.

The current CI workflow only builds identity, trip and chat. Build `web` with
`web/Dockerfile` and empty `VITE_API_BASE_URL`/`VITE_WS_BASE_URL` values so the
browser uses the gateway's same origin.

## 1. Configure the cluster

```bash
cd terraform/cluster
eval "$(terraform output -raw update_kubeconfig_command)"
kubectl get nodes
```

Install the Gateway API standard CRDs (change the version if your selected
Consul chart documents a different supported release), create Consul's gossip
encryption key, and install Consul:

```bash
kubectl apply -f kubernetes/consul/templates/namespaces.yaml
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/experimental-install.yaml
kubectl -n consul create secret generic consul-gossip-encryption-key \
  --from-literal=key="$(openssl rand -base64 32)"
helm repo add hashicorp https://helm.releases.hashicorp.com
helm repo update
helm upgrade --install consul hashicorp/consul \
  --namespace consul \
  --values kubernetes/consul/values.yaml \
  --wait --timeout 15m
kubectl wait --for=condition=Established crd/serviceintentions.consul.hashicorp.com --timeout=5m
```

## 2. Create runtime secrets

Do not commit the following values. The application Secret contains only
connection strings and the internal API token; the JWT private key is mounted
only into identity as a separate Secret.

```bash
kubectl -n togethergo create secret generic togethergo-runtime \
  --from-literal=IDENTITY_DATABASE_URL='postgresql+asyncpg://USER:PASSWORD@HOST:5432/identity_db' \
  --from-literal=TRIP_DATABASE_URL='postgres://USER:PASSWORD@HOST:5432/trip_db?sslmode=require' \
  --from-literal=CHAT_DATABASE_URL='postgres://USER:PASSWORD@HOST:5432/chat_db?sslmode=require' \
  --from-literal=RABBITMQ_URL='amqps://USER:PASSWORD@HOST:5671/VHOST' \
  --from-literal=INTERNAL_API_TOKEN='REPLACE_WITH_AT_LEAST_16_RANDOM_CHARACTERS'

kubectl -n togethergo create secret generic togethergo-jwt-private-key \
  --from-file=jwt_private.pem=deploy/keys/jwt_private.pem
```

For production, prefer External Secrets or the Secrets Store CSI driver over
manually created Kubernetes Secrets. `secrets.existingSecret` lets the chart use
such a Secret without rendering credentials into a Helm release.

## 3. Add ECR images and environment values

```bash
cp kubernetes/values.example.yaml kubernetes/values.local.yaml
$EDITOR kubernetes/values.local.yaml
```

Replace each ECR repository and tag, set the Redis endpoint, and change
`chatAllowedOrigins` to the public hostname without a scheme. EKS nodes normally
pull from ECR through their node IAM role, so no `imagePullSecret` is needed for
same-account ECR repositories.

Validate before applying:

```bash
helm lint kubernetes/togethergo -f kubernetes/values.local.yaml
helm template togethergo kubernetes/togethergo \
  -f kubernetes/values.local.yaml > /tmp/togethergo-rendered.yaml
```

## 4. Deploy

The migration Jobs are Helm pre-install/pre-upgrade hooks. They must succeed
before Deployments are changed, and each service receives only its own database
URL. The namespace and runtime Secrets must therefore exist before the first
Helm installation, as created in steps 1 and 2.

```bash
helm upgrade --install togethergo kubernetes/togethergo \
  --namespace togethergo \
  --values kubernetes/values.local.yaml \
  --wait --timeout 15m

kubectl apply -f kubernetes/consul/templates/intentions.yaml
kubectl apply -f kubernetes/consul/templates/gateway.yaml
kubectl apply -f kubernetes/consul/templates/routes.yaml
```

Find the public load balancer and point DNS at it:

```bash
kubectl -n consul get gateway togethergo-gateway
kubectl -n consul get services
```

After DNS is working, add the hostname to the listener in
`consul/templates/gateway.yaml`. TLS can then be added with a Gateway HTTPS
listener and a certificate Secret; the Terraform ACM certificate cannot be
attached directly to Consul's Envoy gateway as a Kubernetes TLS Secret.

## 5. Verify and operate

```bash
kubectl -n togethergo get pods,services,hpa,jobs
kubectl -n togethergo get httproutes
kubectl -n togethergo get serviceintentions
kubectl -n togethergo rollout status deployment/identity
kubectl -n togethergo rollout status deployment/trip
kubectl -n togethergo rollout status deployment/chat
kubectl -n togethergo rollout status deployment/web
```

For a new image tag, update `values.local.yaml` and repeat the `helm upgrade`
command. To inspect a failed migration, list Jobs and read its pod logs before
the next upgrade.
