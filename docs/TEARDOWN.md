# Tearing down the TogetherGo cluster

`terraform destroy` on `terraform/cluster` is **not** sufficient, and running it
first is the mistake that costs money. Read this before you run it.

Estimated time for a clean teardown: **20–30 minutes**, most of it waiting.

## Why a plain destroy fails

The AWS Load Balancer Controller runs inside the cluster. When you create an
Ingress, the controller — not Terraform — calls the AWS API and creates an
Application Load Balancer, a target group, and a security group. Terraform has
no record of any of them.

Two things follow:

1. **The destroy hangs.** Terraform tries to delete the VPC's subnets and
   security groups. The ALB's ENIs are still attached to those subnets, and the
   controller's security group has rules referencing the node security group, so
   the deletes are rejected. Terraform retries until it times out, typically on
   `aws_subnet` or `aws_security_group` after ~15 minutes, with
   `DependencyViolation` or `has a dependent object`.

2. **What is left keeps billing.** If you delete the cluster while the
   controller still exists, the controller is gone before it can clean up. The
   ALB (~$0.027/hour), its Elastic IPs, and any `Retain`-policy EBS volumes stay
   in the account with nothing pointing at them. This is the classic "I
   destroyed everything and AWS still charged me $60" case.

The controller must be given the chance to delete what it created, while the
cluster is still up. That is the whole procedure.

## Teardown procedure

### 1. Delete the workload, including its Ingresses

```bash
eval "$(terraform -chdir=terraform/cluster output -raw update_kubeconfig_command)"

helm uninstall togethergo --namespace togethergo
kubectl delete namespace togethergo
```

Deleting the Ingress objects is what makes the controller delete the ALB. If you
uninstall the controller first, nothing is left to do the cleanup and you are
into the manual path below.

Confirm no Ingress or LoadBalancer Service remains anywhere:

```bash
kubectl get ingress --all-namespaces
kubectl get svc --all-namespaces --field-selector spec.type=LoadBalancer
```

Both must be empty. A `Service` of type `LoadBalancer` creates an NLB or a
classic ELB and has exactly the same problem.

### 2. Wait for the ALB and its security groups to actually disappear

Deletion is asynchronous. The controller sees the Ingress removal within
seconds, but AWS takes a few minutes to detach the ENIs.

```bash
CLUSTER=togethergo-dev
REGION=eu-central-1

# Poll until this prints nothing.
aws elbv2 describe-load-balancers --region "$REGION" \
  --query "LoadBalancers[?contains(LoadBalancerName, '$CLUSTER')].[LoadBalancerName,State.Code]" \
  --output text

# The controller's own security groups are tagged with the cluster name.
aws ec2 describe-security-groups --region "$REGION" \
  --filters "Name=tag:elbv2.k8s.aws/cluster,Values=$CLUSTER" \
  --query 'SecurityGroups[].[GroupId,GroupName]' --output text
```

Do not move on until both commands return empty. This usually takes 2–5 minutes.

### 3. Delete PersistentVolumeClaims

Postgres, Redis and RabbitMQ run as StatefulSets. Deleting a StatefulSet does
**not** delete its PVCs — that is deliberate on Kubernetes' part, so you do not
lose a database by scaling to zero. Here it means orphaned EBS volumes.

```bash
kubectl get pvc --all-namespaces
kubectl delete pvc --all --namespace togethergo
```

The `gp3` StorageClass uses `reclaimPolicy: Delete`, so removing the PVC removes
the EBS volume — but only while the EBS CSI driver is still running. After the
cluster is gone, the volumes are yours to find by hand.

**If you want to keep the data**, snapshot first:

```bash
kubectl get pv -o jsonpath='{range .items[*]}{.spec.csi.volumeHandle}{"\n"}{end}'
aws ec2 create-snapshot --region "$REGION" --volume-id vol-xxxxxxxx \
  --description "togethergo postgres pre-teardown"
```

### 4. Uninstall the load balancer controller

```bash
helm uninstall aws-load-balancer-controller --namespace kube-system
```

Only now, after it has finished its own cleanup.

### 5. Destroy the cluster module

```bash
cd terraform/cluster
terraform destroy
```

10–15 minutes. Should complete with no retries. If it hangs on a subnet or
security group, something from step 2 is still alive — cancel, go back, and
check again.

### 6. Verify nothing is left billing

Terraform reporting success is not proof the account is clean. Check the four
things that survive a botched destroy. Console links assume `eu-central-1`.

**Load balancers** — EC2 → Load Balancers
(`https://eu-central-1.console.aws.amazon.com/ec2/home?region=eu-central-1#LoadBalancers:`)

```bash
aws elbv2 describe-load-balancers --region "$REGION" \
  --query 'LoadBalancers[].[LoadBalancerName,VpcId,CreatedTime]' --output table
aws elb describe-load-balancers --region "$REGION" \
  --query 'LoadBalancerDescriptions[].[LoadBalancerName,VPCId]' --output table   # classic
```

Anything in the VPC you just deleted is an orphan. Delete it:
`aws elbv2 delete-load-balancer --load-balancer-arn <arn>`.

**Target groups** — cheap but they accumulate and hit the per-region quota:

```bash
aws elbv2 describe-target-groups --region "$REGION" \
  --query 'TargetGroups[?LoadBalancerArns==`[]`].[TargetGroupName]' --output text
```

**EBS volumes** — EC2 → Volumes, filter State = `available`
(`https://eu-central-1.console.aws.amazon.com/ec2/home?region=eu-central-1#Volumes:`)

```bash
aws ec2 describe-volumes --region "$REGION" \
  --filters Name=status,Values=available \
  --query 'Volumes[].[VolumeId,Size,CreateTime,Tags[?Key==`kubernetes.io/created-for/pvc/name`].Value|[0]]' \
  --output table
```

An `available` volume is attached to nothing and bills at full rate — about
$0.10 per GB per month, so a forgotten 20 GB Postgres volume is $2/month
forever. Delete with `aws ec2 delete-volume --volume-id vol-xxxxxxxx`.

**Elastic IPs** — EC2 → Elastic IPs
(`https://eu-central-1.console.aws.amazon.com/ec2/home?region=eu-central-1#Addresses:`)

```bash
aws ec2 describe-addresses --region "$REGION" \
  --query 'Addresses[?AssociationId==`null`].[PublicIp,AllocationId]' --output table
```

An unassociated Elastic IP costs about $3.60/month — AWS bills idle addresses
specifically to discourage hoarding. Release with
`aws ec2 release-address --allocation-id eipalloc-xxxxxxxx`.

**Network interfaces**, if the VPC itself refused to delete:

```bash
aws ec2 describe-network-interfaces --region "$REGION" \
  --filters Name=vpc-id,Values=<vpc-id> \
  --query 'NetworkInterfaces[].[NetworkInterfaceId,Description,Status]' --output table
```

**CloudWatch log group** — `terraform destroy` removes
`/aws/eks/togethergo-dev/cluster`, but if the destroy was interrupted it can be
left behind holding retained log data:

```bash
aws logs describe-log-groups --region "$REGION" \
  --log-group-name-prefix /aws/eks/ --query 'logGroups[].logGroupName' --output text
```

## What deliberately survives

`terraform/bootstrap` is untouched by any of this:

- the state bucket and its version history
- the DynamoDB lock table
- **all five ECR repositories and every image in them** — `force_delete` is
  `false` on the repositories, so even an explicit destroy of the bootstrap
  module refuses while images exist
- the GitHub OIDC provider and the CI role

So the next `terraform apply` in `terraform/cluster` produces a working cluster
that can immediately pull the images you already built. Rebuilding images is
never part of recreating the cluster.

## Recovering from a destroy you ran in the wrong order

If you already ran `terraform destroy` with the controller still installed, and
it failed partway:

1. Find the VPC id in the state: `terraform state show module.vpc.aws_vpc.this[0] | grep '^ *id'`
2. Delete the load balancers in that VPC by hand (step 6 above).
3. Delete their security groups — you may have to remove cross-referencing
   ingress rules from the node security group first, since two groups that
   reference each other cannot be deleted in either order.
4. Re-run `terraform destroy`. It is idempotent; it will pick up where it stopped.
5. Sweep for orphaned volumes and Elastic IPs, which will *not* be in state and
   which nothing will ever clean up for you.
