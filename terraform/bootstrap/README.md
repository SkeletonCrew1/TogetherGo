# bootstrap

Account-level scaffolding that outlives any particular cluster: the Terraform
state backend, the container registries, and the identity GitHub Actions
assumes. Apply this once. It is not part of the normal create/destroy cycle.

## What it creates

| Resource | Notes |
| --- | --- |
| S3 bucket `togethergo-tfstate-<account-id>` | versioned, SSE-S3, all public access blocked, noncurrent versions expire after 90 days, `prevent_destroy` |
| DynamoDB table `togethergo-tf-lock` | `PAY_PER_REQUEST`, `prevent_destroy` |
| 5 ECR repositories | `togethergo/{identity,trip,chat,notification,web}`, immutable tags, scan on push, keep last 30 images |
| GitHub OIDC provider | `token.actions.githubusercontent.com` |
| IAM role `togethergo-github-actions` | assumable only from the configured repository; ECR push + `eks:DescribeCluster` |

19 resources in total.

## Apply

```bash
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars          # github_repository is required
terraform init
terraform apply
```

**Expected duration: 1–2 minutes.** Nothing here waits on a slow control plane.

The first apply necessarily runs on local state, because it is creating the
bucket that state will live in.

## One-time migration to S3

After the first successful apply:

```bash
terraform output -raw state_bucket_name
```

Uncomment the `backend "s3"` block in [`backend.tf`](backend.tf), fill in that
bucket name, then:

```bash
terraform init -migrate-state     # answer "yes" when asked to copy state
terraform plan                    # must report: No changes.
rm terraform.tfstate terraform.tfstate.backup
```

Deleting the local state afterwards matters. A stale `terraform.tfstate` sitting
in this directory is how someone later applies from a six-month-old view of the
account and deletes the registries.

## Wiring GitHub Actions

```yaml
permissions:
  id-token: write        # required, or the OIDC token is never minted
  contents: read

steps:
  - uses: aws-actions/configure-aws-credentials@v4
    with:
      role-to-assume: <terraform output -raw ci_role_arn>
      aws-region: eu-central-1
  - uses: aws-actions/amazon-ecr-login@v2
```

The role's trust policy is scoped to `repo:<owner>/<name>:*`. To restrict deploys
to one branch, set `github_subject_claims` instead:

```hcl
github_subject_claims = ["repo:your-org/TogetherGo:ref:refs/heads/main"]
```

The role can push images and call `eks:DescribeCluster`. It is deliberately not
a cluster admin: authorisation *inside* the cluster is a separate EKS access
entry, granted when you decide CI should be allowed to run `helm upgrade`.

Image tags are immutable, so CI must tag with the commit SHA. Pushing
`:latest` twice fails with `ImageTagAlreadyExistsException`, which is the point.

## Estimated cost

Roughly **$1–5 / month**, effectively all of it ECR storage at $0.10 per GB per
month. State objects are kilobytes; DynamoDB on-demand billing for a few dozen
lock acquisitions per month rounds to zero.

## Destroying

Don't, in the normal course of things. The state bucket and lock table carry
`prevent_destroy`, so `terraform destroy` fails on them by design. Removing this
module for real means deleting the ECR images, emptying the versioned bucket,
and dropping both `prevent_destroy` blocks — three deliberate steps, which is
the right amount of friction for something that holds the record of your account.
