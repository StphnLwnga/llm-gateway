# 0006. Deploy targets: Compose, kind, EKS, GKE Autopilot, via Terraform

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: Compose. Milestone 8 (planned): kind with a Kustomize base. Milestone 12 (planned): EKS. Milestone 13 (planned): GKE Autopilot.

## Context

The gateway runs as two processes (`cmd/gateway`, `cmd/worker`) with Postgres, Redis and Kafka. It must run on a laptop for development and in Kubernetes for production. The manifests should not fork per environment, and cloud cost must stay low while the deployment is built and measured.

## Considered options

1. Compose locally, kind in CI, EKS and GKE Autopilot in the cloud. One Kustomize base with an overlay per target. Cloud infrastructure in Terraform.
2. Compose locally and one cloud only.
3. A container platform without Kubernetes, for example Cloud Run or ECS on Fargate.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| Compose, kind, EKS, GKE via Terraform | One Kustomize base runs on the three Kubernetes targets, so any target-specific setting is visible in its overlay. The same workload is measured on two clouds, so the cost comparison uses real numbers. Terraform creates and destroys each cloud stack the same way every time. | Two cloud stacks to write and keep working. Each cluster has a fee while it exists. |
| One cloud | Half the infrastructure code. | Portability is assumed, not tested. No cost comparison. |
| No Kubernetes | No cluster to operate. | The drain and scaling design (readiness drain, PodDisruptionBudget, HPA) does not carry over as built. Kafka needs a separate managed service. A different model from the kind CI target. |

## Decision outcome

Chosen: option 1.

- **Compose** (`deploy/compose.yaml`, built in M0): Postgres, the migration step and the gateway.
- **kind** (M8): the Kustomize base and a kind overlay, Strimzi for Kafka, probes, PDB and HPA. A CI job runs a smoke test with a streamed request.
- **EKS** (M12) and **GKE Autopilot** (M13): an overlay each, with Terraform for the cluster, registry, database and key management.
- Each cloud stack is applied, measured and destroyed the same day. EKS and GKE both charge $0.10 per cluster per hour for the cluster. GKE's free tier gives $74.40 in monthly credits per billing account, which covers one Autopilot or zonal cluster.

## Consequences

- Good: the drain, probe and shutdown settings ([0008](0008-graceful-shutdown.md)) are tested on kind before they reach a cloud.
- Good: cloud spend is bounded by the hours a stack exists, and milestones 12 and 13 record the actual cost.
- Bad: no cloud environment stays up between test runs, so every cloud test pays the cluster creation time.
- Bad: Strimzi on EKS vs Amazon MSK is a separate cost decision, made in milestone 12.

## Links

- Kustomize bases and overlays: https://kubectl.docs.kubernetes.io/references/kustomize/glossary/
- kind: https://kind.sigs.k8s.io/
- Amazon EKS pricing: https://aws.amazon.com/eks/pricing/
- GKE pricing: https://cloud.google.com/kubernetes-engine/pricing
- GKE Autopilot overview: https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-overview
- Terraform: https://developer.hashicorp.com/terraform
