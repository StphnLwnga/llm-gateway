# 0008. Graceful shutdown: readiness drain, Shutdown, then Close

- Status: Accepted
- Date: 2026-10-10
- Decision-makers: Stephen Lwanga
- Implemented in: Milestone 0: 392ed3f, 796bff8. Milestone 8 (planned): probe timing and the deployment's drain settings.

## Context

The gateway serves long streamed responses. When Kubernetes deletes a pod, two things start in parallel. The control plane marks the pod's endpoint as terminating in its EndpointSlice. The Kubernetes docs say this condition "is set when the Pod is first deleted (that is, when it receives a deletion timestamp, but most likely before the Pod's containers exit)." The kubelet sends SIGTERM to the container at the same time. The endpoint update reaches kube-proxy on each node, and each ingress controller, over some seconds. A node that has not caught up still routes new requests to the pod.

Kubernetes sends SIGKILL when the grace period ends (`terminationGracePeriodSeconds`, 30 seconds by default). SIGKILL cannot be caught, and it drops every open stream.

## Considered options

1. In-app readiness drain: on SIGTERM, `/readyz` returns 503 and the server keeps serving for `DRAIN_DELAY`. Then `http.Server.Shutdown` waits up to `SHUTDOWN_TIMEOUT` for in-flight requests, then `Close` cuts what is left.
2. Immediate `Shutdown` on SIGTERM.
3. A `preStop` hook that sleeps before SIGTERM, with no delay in the app.

## Pros and cons

| Option | Pros | Cons |
|---|---|---|
| In-app drain, Shutdown, Close | New requests routed by a late node are still served. `/readyz` 503 also removes the pod from load balancers that health-check the pod directly, and when every endpoint is terminating. Tested in Go (`TestReadyzIs503DuringTheDrainDelay`). | Shutdown takes `DRAIN_DELAY` longer. The 503 acts only after the probe fails `failureThreshold` times. |
| Immediate Shutdown | Fastest exit. | The listener closes while some nodes still route to the pod, so those clients get "connection refused". |
| preStop sleep | No code. Works for any image. | Needs a `sleep` in the image or the `sleep` lifecycle action. `/readyz` stays 200 during the sleep, so a load balancer that probes the pod keeps sending traffic. |

## Decision outcome

Chosen: option 1, in `cmd/gateway/serve.go`.

```text
SIGTERM ─> draining: /readyz 503, still serving ─> DRAIN_DELAY ends
        ─> Shutdown: listener closed, in-flight requests finish (up to SHUTDOWN_TIMEOUT)
        ─> clean: exit 0
        ─> timeout: Close cuts the rest, errors joined, exit 1
```

- **The budget.** `DRAIN_DELAY` (default 0s locally, 5s planned in Kubernetes) plus `SHUTDOWN_TIMEOUT` (default 20s) must stay below the grace period: 5s + 20s = 25s, under 30s.
- **Request contexts are not derived from the signal context.** The server keeps its default `BaseContext`. A request context derived from the signal context would end every in-flight stream on SIGTERM. The in-flight shutdown test uses a handler that watches its request context, so it fails if this changes.
- **The shutdown deadline keeps the context's values.** `context.WithoutCancel(ctx)` with its own timeout, because `ctx` is already cancelled when shutdown starts.
- **A second signal quits at once.** `context.AfterFunc(ctx, stop)` restores the default signal behaviour right after the first signal, so a second Ctrl-C or SIGTERM ends the process without waiting for the drain.
- **A timed-out shutdown is reported.** `Shutdown`'s error is joined with `Close`'s, and the process exits 1, so the logs show that requests were cut.

## Consequences

- Good: an in-flight stream survives SIGTERM, and a request that a late node routes to the pod during `DRAIN_DELAY` gets 200.
- Bad: the Kubernetes probe defaults (`periodSeconds` 10, `failureThreshold` 3) can keep a pod ready for about 30 seconds, longer than a 5s `DRAIN_DELAY`. The 503 then matters only after the delay. Milestone 8 sets the probe timing to fit inside the delay, and milestones 12 and 13 do the same for cloud load balancer health checks.
- Bad: Milestone 8 still decides whether the deployment uses the in-app delay, a `preStop` sleep, or both.

## Links

- Kubernetes, Pod termination: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination
- Kubernetes, EndpointSlice conditions (serving, terminating): https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/
- Kubernetes, probe settings and defaults: https://kubernetes.io/docs/concepts/workloads/pods/probes/
- `net/http.Server.Shutdown`: https://pkg.go.dev/net/http#Server.Shutdown
- `context.WithoutCancel`: https://pkg.go.dev/context#WithoutCancel
- `context.AfterFunc`: https://pkg.go.dev/context#AfterFunc
- Code: `cmd/gateway/serve.go`, `cmd/gateway/main.go`, `cmd/gateway/serve_test.go`
