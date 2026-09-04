# cluster

The disposable half: VPC, EKS, the IAM roles that in-cluster controllers assume,
and optionally a TLS certificate. Destroy and recreate this as often as you
like — but read [`docs/TEARDOWN.md`](../../docs/TEARDOWN.md) before the first
destroy, because the load balancer is not Terraform's to delete.

Requires `bootstrap` to have been applied first: the backend config comes from
its outputs.

## What it creates

| | |
| --- | --- |
| VPC | `10.42.0.0/16` across 2 AZs, 2 private `/20` (nodes), 2 public `/24` (ALB) |
| NAT | one gateway, shared by both AZs |
| S3 gateway endpoint | keeps ECR layer pulls off the NAT |
| EKS | v1.31, public endpoint restricted by CIDR, IRSA on |
| Node group | 2 × `t4g.medium`, `AL2023_ARM_64_STANDARD`, min 2 / desired 2 / max 4, 40 GB gp3 root |
| Addons | vpc-cni, coredns, kube-proxy, aws-ebs-csi-driver |
| IRSA roles | AWS Load Balancer Controller, EBS CSI driver |
| ACM | certificate + Route 53 validation, behind `create_certificate` |

66 resources with TLS off, 69 with it on (measured from `terraform plan`).

## Apply

```bash
terraform -chdir=../bootstrap output -raw backend_config > backend.hcl
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars
terraform init -backend-config=backend.hcl
terraform apply
```

**Expected duration: 15–20 minutes.** The EKS control plane alone is 9–11
minutes of it; the node group is another 3–4, and the EBS CSI addon waits for
nodes to be ready before it reports `ACTIVE`. Destroy is 10–15 minutes.

To validate without credentials or a backend:

```bash
terraform init -backend=false && terraform validate
```

## Then, outside Terraform

Three things have to happen inside the cluster before the Helm chart will work.
None of them belong in this state.

```bash
# 1. Point kubectl at the cluster.
eval "$(terraform output -raw update_kubeconfig_command)"

# 2. Default StorageClass. EKS ships gp2 as default; two defaults is an error
#    state, so gp3 is added and gp2 demoted in the same step.
kubectl apply -f manifests/gp3-storageclass.yaml
kubectl patch storageclass gp2 \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}}'
kubectl get storageclass          # exactly one (default)

# 3. AWS Load Balancer Controller, bound to the IRSA role created here.
terraform output -raw alb_controller_helm_command   # copy-pasteable
```

The StorageClass uses `WaitForFirstConsumer` binding. This is not a detail: with
`Immediate` binding the Postgres PVC gets a volume in whichever AZ the
provisioner picks, and if the scheduler later places the pod in the other AZ, an
EBS volume cannot follow it. The pod sits `Pending` forever with a message about
node affinity. `WaitForFirstConsumer` schedules the pod first, then creates the
volume next to it.

## TLS

Off by default so the module applies for someone who does not own a domain; the
ALB then serves plain HTTP. To turn it on you need a Route 53 hosted zone in the
same account:

```hcl
create_certificate        = true
domain_name               = "togethergo.example.com"
subject_alternative_names = ["*.togethergo.example.com"]
hosted_zone_id            = "Z0123456789ABCDEFGHIJ"
```

A wildcard SAN validates through the same DNS record as its parent domain, so
listing both produces one Route 53 record, not two fighting over the same name.
Apply blocks on `aws_acm_certificate_validation` until ACM sees the record —
usually under a minute, occasionally several.

Feed the ARN to the Ingress:

```yaml
alb.ingress.kubernetes.io/certificate-arn: <terraform output -raw certificate_arn>
```

## Cost

Approximate, eu-central-1, on-demand, running continuously:

| Item | $/month |
| --- | ---: |
| EKS control plane | 73 |
| 2 × t4g.medium | 58 |
| NAT gateway (hourly) | 38 |
| ALB (created by Helm, not here) | 20 |
| EBS: 2 × 40 GB root + ~30 GB of PVCs | 11 |
| CloudWatch control plane logs | 3 |
| **Total** | **~200** |

Plus data processing: $0.052/GB through the NAT and per-LCU on the ALB. The S3
gateway endpoint exists specifically to keep image pulls out of the first
number.

The control plane bills whether or not anything is scheduled on it, so an idle
cluster left up over a weekend is about $20. `terraform destroy` when you are
done for the day — that is what the two-module split is for.

## Endpoint exposure

`cluster_endpoint_public_access_cidrs` defaults to `0.0.0.0/0` so that a first
apply works from anywhere. The API is still authenticated, but there is no
reason to leave it advertised to the internet. Narrow it once you know your
egress address:

```hcl
cluster_endpoint_public_access_cidrs = ["203.0.113.4/32"]
```
