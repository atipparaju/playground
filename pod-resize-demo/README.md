# pod-resize-demo

A Go sample application demonstrating **Kubernetes In-Place Pod Vertical Scaling**
(`InPlacePodVerticalScaling` feature gate), which allows changing a container's
CPU and memory resources without recreating the pod.

Reference: https://devopscube.com/vpa-in-place-pod-resize/

---

## What This Demo Shows

| Topic | Detail |
|---|---|
| Feature gate | `InPlacePodVerticalScaling` (alpha K8s 1.27, stable K8s 1.29) |
| CPU resize | Hot-plug via cgroups – **no container restart** |
| Memory resize | Requires **container restart** (pod UID unchanged) |
| Resize status | `status.resize`: `Proposed` → `InProgress` → *(empty)* |
| Actual allocation | `status.containerStatuses[].resources` (live view) |
| `/resize` subresource | New API endpoint used to trigger the resize |

---

## Repository Layout

```
pod-resize-demo/
├── main.go               # Go HTTP server (resource reporter + memory allocator)
├── go.mod
├── Dockerfile            # Multi-stage build → alpine image
├── demo.sh               # Interactive end-to-end walkthrough script
└── k8s/
    ├── deployment.yaml   # Deployment with resizePolicy configured
    ├── service.yaml      # ClusterIP service
    ├── pod-direct.yaml   # Standalone pod for direct resize API demos
    ├── resize-cpu-up.yaml     # Deployment patch: scale CPU up
    ├── resize-cpu-down.yaml   # Deployment patch: scale CPU back down
    └── resize-memory-up.yaml  # Deployment patch: scale memory up
```

---

## Prerequisites

- Kubernetes **>= 1.27** with `InPlacePodVerticalScaling` enabled.
  - On K8s **>= 1.29** the gate is on by default.
  - For kubeadm, add to `kubelet` and `kube-apiserver` configs:
    ```
    --feature-gates=InPlacePodVerticalScaling=true
    ```
- `kubectl` configured and pointing at your cluster.
- Docker (or compatible container runtime) for building the image.

---

## Quick Start

### 1. Build and Load the Image

```bash
cd pod-resize-demo

# Build
docker build -t pod-resize-demo:latest .

# For kind clusters
kind load docker-image pod-resize-demo:latest

# For minikube
minikube image load pod-resize-demo:latest
```

### 2. Run the Automated Demo

```bash
./demo.sh
```

The script walks through every step interactively, pausing for you to inspect
the cluster state between stages.

### 3. Manual Steps

#### Deploy

```bash
kubectl apply -f k8s/pod-direct.yaml
kubectl wait pod/pod-resize-demo-direct --for=condition=Ready --timeout=120s
```

#### Inspect Initial Resources

```bash
# What Kubernetes has allocated (live)
kubectl get pod pod-resize-demo-direct \
  -o jsonpath='{.status.containerStatuses[0].resources}' | jq .

# resizePolicy declared on the container
kubectl get pod pod-resize-demo-direct \
  -o jsonpath='{.spec.containers[0].resizePolicy}' | jq .
```

#### Resize CPU (no restart)

```bash
kubectl patch pod pod-resize-demo-direct \
  --subresource resize \
  --type merge \
  -p '{
    "spec": {
      "containers": [{
        "name": "app",
        "resources": {
          "requests": {"cpu": "300m"},
          "limits":   {"cpu": "500m"}
        }
      }]
    }
  }'

# Watch the resize status
watch kubectl get pod pod-resize-demo-direct \
  -o jsonpath='{.status.resize}'

# Verify restart count is unchanged
kubectl get pod pod-resize-demo-direct \
  -o jsonpath='{.status.containerStatuses[0].restartCount}'
```

#### Resize Memory (container restart)

```bash
kubectl patch pod pod-resize-demo-direct \
  --subresource resize \
  --type merge \
  -p '{
    "spec": {
      "containers": [{
        "name": "app",
        "resources": {
          "requests": {"memory": "256Mi"},
          "limits":   {"memory": "512Mi"}
        }
      }]
    }
  }'

# Restart count increments; pod UID stays the same
kubectl get pod pod-resize-demo-direct -o jsonpath='{.status.containerStatuses[0].restartCount}'
kubectl get pod pod-resize-demo-direct -o jsonpath='{.metadata.uid}'
```

#### Resize via Deployment Patch

```bash
kubectl apply -f k8s/deployment.yaml
kubectl apply -f k8s/service.yaml

# Scale CPU up (no restart)
kubectl patch deployment pod-resize-demo \
  --patch-file k8s/resize-cpu-up.yaml

# Scale memory up (triggers container restart via RestartContainer policy)
kubectl patch deployment pod-resize-demo \
  --patch-file k8s/resize-memory-up.yaml
```

---

## App Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/` | GET | JSON snapshot: hostname, Go runtime memory stats, cgroup limits, manually allocated MiB |
| `/healthz` | GET | Liveness probe (`ok`) |
| `/alloc?mib=N` | POST | Allocate N MiB (1–512) and hold the reference to simulate memory pressure |
| `/free` | POST | Release all manually allocated memory |

### Example Response (`/`)

```json
{
  "hostname": "pod-resize-demo-direct",
  "timestamp": "2025-01-15T10:30:00Z",
  "go_version": "go1.22.0",
  "cgroup_limits": {
    "cpu_limit": "0.200 cores",
    "memory_limit": "256 MiB",
    "cpu_request_env": "100m",
    "mem_limit_env": "268435456"
  },
  "runtime_memory": {
    "alloc_bytes": 1048576,
    "total_alloc_bytes": 2097152,
    "sys_bytes": 8388608,
    "num_gc": 3
  },
  "manually_allocated_mib": 0,
  "resize_feature": "InPlacePodVerticalScaling"
}
```

After a CPU resize the `cgroup_limits.cpu_limit` field updates to reflect the
new cgroup quota – with no container restart.

---

## Key Kubernetes Concepts

### `resizePolicy`

Declared per container, per resource type:

```yaml
resizePolicy:
  - resourceName: cpu
    restartPolicy: NotRequired      # hot-plug via cgroup update
  - resourceName: memory
    restartPolicy: RestartContainer # container restarts; pod stays on same node
```

### Resize Status (`status.resize`)

| Value | Meaning |
|---|---|
| `Proposed` | API server accepted; kubelet not yet acted |
| `InProgress` | kubelet is applying the resize |
| `Deferred` | Node lacks capacity; will retry when resources free up |
| `Infeasible` | Resize cannot be satisfied (e.g. exceeds node allocatable) |
| *(empty)* | Resize complete |

### `/resize` Subresource

The `PATCH /api/v1/namespaces/{ns}/pods/{name}/resize` endpoint is the
dedicated API for triggering in-place resizes.  It validates the request
before the kubelet acts, giving an immediate error for invalid values.

---

## Cleanup

```bash
kubectl delete -f k8s/pod-direct.yaml --ignore-not-found
kubectl delete -f k8s/deployment.yaml --ignore-not-found
kubectl delete -f k8s/service.yaml    --ignore-not-found
```
