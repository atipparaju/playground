#!/usr/bin/env bash
# demo.sh – End-to-end walkthrough of Kubernetes In-Place Pod Vertical Scaling.
#
# Prerequisites
#   • Kubernetes >= 1.27 with InPlacePodVerticalScaling feature gate enabled
#     (enabled by default in K8s >= 1.29)
#   • kubectl configured and pointing at your cluster
#   • Docker image built locally (see README for build command)
#
# Usage:  ./demo.sh

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
POD_NAME="pod-resize-demo-direct"
CONTAINER="app"

###############################################################################
header() { echo; echo "===================================================="; echo "  $*"; echo "===================================================="; }
step()   { echo; echo "--- $*"; }
pause()  { echo; read -r -p "Press ENTER to continue..."; }
###############################################################################

header "0. Check InPlacePodVerticalScaling feature gate"
kubectl get --raw /version | python3 -c "import sys,json; v=json.load(sys.stdin); print('Server version:', v['gitVersion'])"

step "Verify the feature gate is active (look for InPlacePodVerticalScaling=true)"
kubectl get --raw "/api/v1" | python3 -c "
import sys, json
api = json.load(sys.stdin)
resources = [r['name'] for r in api.get('resources', [])]
print('Pod subresources available:', [r for r in resources if 'resize' in r] or '(not listed separately – check feature gate)')
"

pause

###############################################################################
header "1. Deploy the demo pod"

step "Apply the standalone pod manifest"
kubectl apply -n "$NAMESPACE" -f k8s/pod-direct.yaml

step "Wait for pod to be Running"
kubectl wait -n "$NAMESPACE" pod/"$POD_NAME" \
  --for=condition=Ready \
  --timeout=120s

step "Initial pod resources (spec)"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.spec.containers[0].resources}' | python3 -m json.tool

step "Initial pod resources (status – what is actually allocated)"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].resources}' | python3 -m json.tool

step "resizePolicy on container"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.spec.containers[0].resizePolicy}' | python3 -m json.tool

pause

###############################################################################
header "2. Port-forward and query the app"

step "Starting port-forward in background (localhost:8080 → pod:8080)"
kubectl port-forward -n "$NAMESPACE" pod/"$POD_NAME" 8080:8080 &
PF_PID=$!
sleep 2

step "App status before resize"
curl -s http://localhost:8080/ | python3 -m json.tool

pause

###############################################################################
header "3. In-place CPU resize (no container restart)"

step "Current restart count BEFORE resize"
RESTARTS_BEFORE=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
echo "Restart count: $RESTARTS_BEFORE"

step "Patching pod CPU: 100m→300m request, 200m→500m limit (using /resize subresource)"
kubectl patch pod -n "$NAMESPACE" "$POD_NAME" \
  --subresource resize \
  --type merge \
  -p '{
    "spec": {
      "containers": [{
        "name": "'"$CONTAINER"'",
        "resources": {
          "requests": {"cpu": "300m"},
          "limits":   {"cpu": "500m"}
        }
      }]
    }
  }'

step "Watching resize status (Proposed → InProgress → done)"
for i in $(seq 1 20); do
  STATUS=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
    -o jsonpath='{.status.resize}' 2>/dev/null || echo "")
  echo "  [$(date +%T)] status.resize = '${STATUS:-<empty = resize complete>}'"
  [[ -z "$STATUS" ]] && break
  sleep 2
done

step "Restart count AFTER CPU resize (should be unchanged)"
RESTARTS_AFTER=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
echo "Restart count: $RESTARTS_AFTER"
if [[ "$RESTARTS_BEFORE" == "$RESTARTS_AFTER" ]]; then
  echo "✓ Container was NOT restarted – in-place CPU resize confirmed!"
else
  echo "✗ Unexpected restart (count changed from $RESTARTS_BEFORE to $RESTARTS_AFTER)"
fi

step "Allocated resources after CPU resize"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].resources}' | python3 -m json.tool

step "App status after CPU resize"
curl -s http://localhost:8080/ | python3 -m json.tool

pause

###############################################################################
header "4. In-place memory resize (container restart required)"

step "Current restart count BEFORE memory resize"
RESTARTS_BEFORE=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
echo "Restart count: $RESTARTS_BEFORE"

step "Patching pod memory: 128Mi→256Mi request, 256Mi→512Mi limit"
kubectl patch pod -n "$NAMESPACE" "$POD_NAME" \
  --subresource resize \
  --type merge \
  -p '{
    "spec": {
      "containers": [{
        "name": "'"$CONTAINER"'",
        "resources": {
          "requests": {"memory": "256Mi"},
          "limits":   {"memory": "512Mi"}
        }
      }]
    }
  }'

step "Waiting for container to restart (memory resizePolicy=RestartContainer)"
kubectl wait -n "$NAMESPACE" pod/"$POD_NAME" \
  --for=condition=Ready \
  --timeout=120s

step "Restart count AFTER memory resize (should have incremented by 1)"
RESTARTS_AFTER=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
echo "Restart count: $RESTARTS_AFTER"
if [[ "$RESTARTS_AFTER" -gt "$RESTARTS_BEFORE" ]]; then
  echo "✓ Container restarted once for memory resize (pod UID unchanged)"
else
  echo "  (container may still be restarting, check manually)"
fi

step "Pod UID (should be unchanged – pod was never rescheduled)"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" -o jsonpath='{.metadata.uid}'
echo

step "Allocated resources after memory resize"
kubectl get pod -n "$NAMESPACE" "$POD_NAME" \
  -o jsonpath='{.status.containerStatuses[0].resources}' | python3 -m json.tool

pause

###############################################################################
header "5. Test memory pressure (allocate memory via /alloc endpoint)"

step "Allocate 100 MiB inside the container"
curl -s -X POST "http://localhost:8080/alloc?mib=100"

step "App memory stats after allocation"
curl -s http://localhost:8080/ | python3 -c "
import sys, json
d = json.load(sys.stdin)
print('Alloc (Go runtime):', d['runtime_memory']['alloc_bytes'] // (1024*1024), 'MiB')
print('Manually allocated :', d['manually_allocated_mib'], 'MiB')
"

step "Free allocated memory"
curl -s -X POST "http://localhost:8080/free"

pause

###############################################################################
header "6. Cleanup"

kill "$PF_PID" 2>/dev/null || true
kubectl delete -n "$NAMESPACE" -f k8s/pod-direct.yaml --ignore-not-found

echo
echo "Demo complete!"
