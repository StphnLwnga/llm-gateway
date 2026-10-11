# 0004. Kafka (KRaft, franz-go) for the usage pipeline

- Status: Accepted
- Date: 2026-10-10
- Decided by: Stephen Lwanga
- Implemented in: S6 (planned): outbox relay, topics, consumer groups. S8 (planned): Strimzi on kind. S12 (planned): Strimzi on EKS vs Amazon MSK.

## Context

Every request produces a usage event: tenant, model, tokens, price charged, latency. Some tenants also opt in to a redacted request log. These events leave the request path through a transactional outbox (S6), so the client never waits for them. Three readers need the same events:

- **billing**, which writes the Postgres ledger, idempotent on `request_id`
- **analytics**, which aggregates usage for dashboards
- **eval-set building**, which reads the opt-in request log (S9)

Each reader must keep its own position, so a slow or failed reader does not hold back the others. The ledger must be rebuildable from the events. Events for one tenant must stay in order.

The current event volume is small (see the estimates in [docs/design.md](../design.md#2-back-of-envelope-estimates)). Throughput does not decide this choice.

## Considered options

1. Apache Kafka in KRaft mode, with the franz-go client.
2. NATS JetStream.

## Pros and cons

| | Kafka (KRaft) | NATS JetStream |
|---|---|---|
| Durable log, independent readers, replay | Yes. Events stay for the topic's retention period, whether or not they are read. Each consumer group commits its own offsets. A new or rebuilt reader replays from any offset. | Yes. A stream keeps messages under its retention limits. Each durable consumer tracks its own position, and a consumer can start from the beginning. |
| Per-tenant order | Events with the same key go to the same partition, and each partition is read in write order. Key: `tenant_id`. | Order per stream or per subject. Per-tenant order needs a subject per tenant or a single ordered consumer. |
| Managed service on AWS | Amazon MSK (provisioned and serverless). | No AWS or Google service. Synadia, the NATS vendor, offers a hosted service and a managed deployment. |
| Managed service on GCP | Google Cloud Managed Service for Apache Kafka, GA since 2024-11-12, runs in KRaft mode. | As above. |
| On Kubernetes | Strimzi operator. Strimzi 0.46 removed ZooKeeper support, so it runs KRaft only. | The NATS Helm chart. Simpler to run. |
| Downstream ecosystem | Kafka Connect for sinks to warehouses and object stores, schema registries for event contracts, Kafka Streams and other stream processors. | Smaller. Fewer ready-made connectors and no common schema registry. |
| Footprint and operation | Heavier: a JVM broker with more memory, and more settings to understand (partitions, replication, retention, consumer rebalancing). KRaft removes ZooKeeper, and Strimzi automates much of the operation. | Lighter: one Go binary (`nats-server -js`), low memory, few settings. Enough for the current volume. |

## Decision outcome

Chosen: Kafka in KRaft mode. Both options meet the log requirements: independent readers with their own positions, replay, and ordered events per tenant. Kafka wins on two points:

1. **Managed services on both cloud targets.** The deploy targets ([0006](0006-deploy-targets.md)) are EKS and GKE. Both clouds offer first-party Kafka services, so the S12 and S13 slices can compare a managed broker with Strimzi on the same protocol and client. JetStream has no first-party service on either cloud.
2. **Downstream consumers.** Usage data feeds billing, analytics and eval sets. Kafka Connect and schema registries let a new consumer, such as a warehouse sink, be added without new gateway code.

Topics:

- `usage.v1`: no prompt content, keyed by `tenant_id`. Consumer groups `billing` and `analytics`.
- `requestlog.v1`: opt-in, redacted before it is produced, short retention.

### Client library

| Client | Pros | Cons |
|---|---|---|
| franz-go | Pure Go, so it builds with `CGO_ENABLED=0` into the static image ([0011](0011-image-and-ci.md)). Supports Kafka 0.8.0 through 4.x, consumer groups with cooperative rebalancing, idempotent and transactional producers. | Community maintained, not backed by Confluent. |
| confluent-kafka-go | Backed by Confluent. Wraps librdkafka, a widely used C client. | Needs cgo: its README says `CGO_ENABLED` "must NOT be set to `0`". That conflicts with the CGO-off static build and the distroless static base image. |
| segmentio/kafka-go | Pure Go, simple API. | Its README says newer Kafka API features "may not yet be implemented", and it does not document idempotent or transactional producers. |

Chosen: franz-go.

## Consequences

- Good: billing, analytics and eval-set building scale and fail independently. A billing bug can be fixed and the ledger rebuilt by replaying `usage.v1`.
- Good: the same client and topics run on Compose (the `apache/kafka` image, KRaft single node), on kind with Strimzi, and on a managed broker.
- Bad: the local stack is heavier. S6 sets Compose memory limits for the broker and measures the whole stack.
- Bad: a large tenant can make its partition hot, because the key is `tenant_id`. S6 measures partition skew.
- Bad: more operating knowledge is needed than for JetStream: partitions, retention, consumer lag and rebalancing. S6 adds a consumer-lag metric.

## Links

- Apache Kafka introduction (retention, partitions, ordering by key): https://kafka.apache.org/intro/
- Amazon MSK: https://docs.aws.amazon.com/msk/latest/developerguide/what-is-msk.html
- Google Cloud Managed Service for Apache Kafka: https://docs.cloud.google.com/managed-service-for-apache-kafka/docs/overview
- Its GA release note (2024-11-12): https://docs.cloud.google.com/managed-service-for-apache-kafka/docs/release-notes
- Strimzi changelog (0.46.0 removes ZooKeeper support): https://github.com/strimzi/strimzi-kafka-operator/blob/main/CHANGELOG.md
- NATS JetStream: https://docs.nats.io/concepts/jetstream
- Synadia Cloud: https://www.synadia.com/cloud
- franz-go: https://github.com/twmb/franz-go
- confluent-kafka-go: https://github.com/confluentinc/confluent-kafka-go
- segmentio/kafka-go: https://github.com/segmentio/kafka-go
