# Consul layer

This directory mirrors the SupernaturalApp layout: `values.yaml` configures the
official HashiCorp Consul chart, while `templates/` contains the TogetherGo
Gateway API routes and service intentions applied after Consul is ready.

The application namespace is `togethergo`. Each workload opts into Connect
injection in the TogetherGo Helm chart. PostgreSQL, Redis and RabbitMQ ports are
excluded from transparent proxying because those dependencies are expected to
be managed outside this Consul catalog.

See [`../README.md`](../README.md) for installation and validation commands.

