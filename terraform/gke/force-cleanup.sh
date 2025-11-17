#!/bin/bash
# Force cleanup GKE cluster resources before terraform destroy

set -e

CLUSTER_NAME="${1:-llmkube-gpu-cluster}"
REGION="${2:-us-west1}"
PROJECT_ID="${3:-llmkube-478121}"

echo "🧹 Force cleaning up GKE cluster: $CLUSTER_NAME"
echo "Region: $REGION"
echo "Project: $PROJECT_ID"
echo ""

# Get cluster credentials
echo "📡 Getting cluster credentials..."
gcloud container clusters get-credentials "$CLUSTER_NAME" \
  --region="$REGION" \
  --project="$PROJECT_ID" 2>/dev/null || echo "Cluster may not be accessible, continuing..."

# Delete all workloads in all namespaces
echo ""
echo "🗑️  Deleting all workloads..."
kubectl delete all --all --all-namespaces --timeout=60s 2>/dev/null || true

# Delete all PVCs (these block node deletion)
echo ""
echo "💾 Deleting all PersistentVolumeClaims..."
kubectl delete pvc --all --all-namespaces --timeout=60s 2>/dev/null || true

# Delete all services with load balancers (these create GCP resources)
echo ""
echo "🌐 Deleting all LoadBalancer services..."
kubectl delete svc --all --all-namespaces --field-selector spec.type=LoadBalancer --timeout=60s 2>/dev/null || true

# Remove finalizers from stuck resources
echo ""
echo "🔓 Removing finalizers from stuck namespaces..."
for ns in $(kubectl get ns --no-headers 2>/dev/null | grep -v "default\|kube-" | awk '{print $1}'); do
  echo "  - Cleaning namespace: $ns"
  kubectl patch namespace "$ns" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
  kubectl delete namespace "$ns" --timeout=30s 2>/dev/null || true
done

# Wait a bit for cleanup
echo ""
echo "⏳ Waiting 30 seconds for cleanup to propagate..."
sleep 30

# Now try terraform destroy
echo ""
echo "🔥 Running terraform destroy..."
cd "$(dirname "$0")"
terraform destroy -auto-approve

# If terraform destroy fails, force delete via gcloud
if [ $? -ne 0 ]; then
  echo ""
  echo "⚠️  Terraform destroy failed. Attempting force delete via gcloud..."
  echo ""

  gcloud container clusters delete "$CLUSTER_NAME" \
    --region="$REGION" \
    --project="$PROJECT_ID" \
    --quiet

  if [ $? -eq 0 ]; then
    echo ""
    echo "✅ Cluster force deleted via gcloud!"
    echo ""
    echo "⚠️  Note: You may need to manually clean up terraform state:"
    echo "    terraform state rm google_container_cluster.gpu_cluster"
    echo "    terraform state rm google_container_node_pool.gpu_pool"
  fi
fi

echo ""
echo "✅ Cleanup complete!"
