# TogetherGo — AWS infrastructure

Terraform for the AWS side of the simplified EKS deployment. **Infrastructure
only.** Postgres, Redis and RabbitMQ run in-cluster as StatefulSets; the
application, its data stores and the AWS Load Balancer Controller are all
installed with Helm, separately from this configuration.

There is no `helm` or `kubernetes` provider anywhere in here, on purpose.
Creating a cluster and deploying into it from a single state makes the first
apply unplannable — the Kubernetes provider needs an API endpoint that does not
exist yet — and the final destroy unreliable, because the provider cannot reach
a cluster that has already been deleted. The seam is the cluster boundary.

## Layout

```
terraform/
├── bootstrap/    long-lived. State bucket, lock table, ECR, GitHub OIDC.
├── cluster/      disposable. VPC, EKS, IRSA roles, ACM. Destroy freely.
├── ec2-demo/     temporary. One Compose host, Elastic IP, DNS and HTTPS.
└── modules/
    ├── tfstate-backend/    S3 + DynamoDB
    ├── ecr-repository/     one registry with a retention policy
    └── github-oidc-role/   OIDC provider + CI role
```

Two root modules, two state files. `bootstrap` holds everything whose loss
would hurt: the state bucket itself, and the container images. `cluster` holds
everything that is cheap to recreate and expensive to leave running. You should
be able to destroy `cluster` on a Friday and apply it again on Monday without
touching `bootstrap`.

## Apply order

```bash
# 1. Bootstrap — once per AWS account.
cd terraform/bootstrap
cp terraform.tfvars.example terraform.tfvars   # set github_repository
terraform init
terraform apply
# then follow the state migration in bootstrap/backend.tf

# 2. Cluster — as often as you like.
cd ../cluster
terraform -chdir=../bootstrap output -raw backend_config > backend.hcl
cp terraform.tfvars.example terraform.tfvars
terraform init -backend-config=backend.hcl
terraform apply
```

Tearing down is **not** just `terraform destroy`. Read
[`docs/TEARDOWN.md`](../docs/TEARDOWN.md) first — the load balancer the AWS Load
Balancer Controller creates is invisible to Terraform, and it will block the
destroy and keep billing.

## Conventions

- Terraform >= 1.9, AWS provider pinned to `~> 5.0`.
- `Project=togethergo`, `Environment=dev`, `ManagedBy=terraform` applied to
  every taggable resource through `default_tags`.
- Nodes are Graviton (`t4g.medium`, `AL2023_ARM_64_STANDARD`). Service images
  must be built for `linux/arm64`. To stay on x86, set
  `node_instance_types = ["t3.medium"]` and
  `node_ami_type = "AL2023_x86_64_STANDARD"`.

## No secrets in this configuration

Terraform state is a plaintext record of every value it manages, so application
credentials — the Postgres password, the RabbitMQ password, the identity
service's RS256 signing key, `INTERNAL_API_TOKEN` — are never created here. They
are created out of band against the running cluster:

```bash
kubectl create secret generic togethergo-postgres \
  --namespace togethergo \
  --from-literal=password="$(openssl rand -base64 32)"
```

The only credential this configuration produces at all is an IAM role trust
relationship, which is a policy document rather than a secret.

## Verifying a change

```bash
terraform fmt -check -recursive terraform/
terraform -chdir=terraform/bootstrap validate
terraform -chdir=terraform/cluster validate   # after init -backend=false
```
