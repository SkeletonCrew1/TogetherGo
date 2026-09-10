# Temporary EC2 deployment

This root module creates a small, self-contained AWS environment for running
TogetherGo with Docker Compose: one VPC, public subnet, EC2 instance, Elastic
IP, security group, and an optional Route 53 `A` record. It does not create a
domain registration and it never puts application secrets in Terraform state.

## 1. Provision AWS infrastructure

```bash
cd terraform/ec2-demo
cp terraform.tfvars.example terraform.tfvars
# Edit the region and domain. Add route53_zone_id when Route 53 hosts its DNS.
terraform init
terraform plan
terraform apply
```

Connect with the `session_manager_command` Terraform output, or configure the
two optional SSH variables. Cloud-init installs Docker, Compose, Git, Make and
OpenSSL. Check `/var/log/cloud-init-output.log` if package installation has not
finished when you first connect.

When DNS is hosted outside Route 53, create an `A` record for `domain_name`
using Terraform's `public_ip` output before starting Caddy.

## 2. Deploy the application

Clone or copy this repository into `/opt/togethergo`. Copy `.env` separately;
do not commit it or pass its contents through Terraform. Generate the JWT keys
on the instance if they were not copied:

```bash
cd /opt/togethergo
make keys
```

Set these deployment values in `.env`:

```dotenv
APP_DOMAIN=togethergo.example.com
LETSENCRYPT_EMAIL=admin@example.com
CHAT_ALLOWED_ORIGINS=togethergo.example.com
ENVIRONMENT=demo
```

Then start the stack with the EC2 overlay and run migrations:

```bash
docker compose -f docker-compose.yml -f docker-compose.ec2.yml up -d --build --wait
docker compose -f docker-compose.yml -f docker-compose.ec2.yml run --rm --no-deps identity alembic upgrade head
docker compose -f docker-compose.yml -f docker-compose.ec2.yml run --rm --no-deps trip migrate up
docker compose -f docker-compose.yml -f docker-compose.ec2.yml run --rm --no-deps chat migrate up
```

Caddy obtains and renews the certificate automatically after DNS resolves to
the Elastic IP and ports 80 and 443 are reachable. Only those two ports are
public in the AWS security group; the debug ports still bound by the local
Compose file cannot be reached from the internet.

## Move local state to S3 later

After creating the bucket, replace the local backend in `backend.tf` with your
S3 backend configuration, then migrate the existing state:

```bash
terraform init -migrate-state
```

Back up `terraform.tfstate` before migration. State files and `terraform.tfvars`
are ignored by Git.

## Remove the demo

Application data lives in Docker volumes on the instance root disk. Back up
anything needed, then destroy the infrastructure from the same state directory:

```bash
terraform destroy
```
