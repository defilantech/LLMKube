#!/bin/bash
# Force cleanup EKS cluster resources before terraform destroy

set -e

CLUSTER_NAME="${1:-llmkube-multi-gpu-test}"
REGION="${2:-us-east-1}"

echo "🧹 Force cleaning up EKS cluster: $CLUSTER_NAME"
echo "Region: $REGION"
echo ""

# Get cluster credentials
echo "📡 Getting cluster credentials..."
aws eks update-kubeconfig \
  --region "$REGION" \
  --name "$CLUSTER_NAME" 2>/dev/null || echo "Cluster may not be accessible, continuing..."

# Delete all workloads in all namespaces
echo ""
echo "🗑️  Deleting all workloads..."
kubectl delete all --all --all-namespaces --timeout=60s 2>/dev/null || true

# Delete all PVCs (these block node deletion)
echo ""
echo "💾 Deleting all PersistentVolumeClaims..."
kubectl delete pvc --all --all-namespaces --timeout=60s 2>/dev/null || true

# Delete all services with load balancers (these create AWS resources)
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

# If terraform destroy fails, try to force delete via AWS CLI
if [ $? -ne 0 ]; then
  echo ""
  echo "⚠️  Terraform destroy failed. Attempting force delete via AWS CLI..."
  echo ""

  # Delete node groups first
  echo "Deleting node groups..."
  for ng in $(aws eks list-nodegroups --cluster-name "$CLUSTER_NAME" --region "$REGION" --query 'nodegroups[]' --output text 2>/dev/null); do
    echo "  - Deleting node group: $ng"
    aws eks delete-nodegroup \
      --cluster-name "$CLUSTER_NAME" \
      --nodegroup-name "$ng" \
      --region "$REGION" 2>/dev/null || true
  done

  # Wait for node groups to delete
  echo "Waiting for node groups to delete (this may take 5-10 minutes)..."
  sleep 60

  # Delete cluster
  echo "Deleting EKS cluster..."
  aws eks delete-cluster \
    --name "$CLUSTER_NAME" \
    --region "$REGION" 2>/dev/null || true

  if [ $? -eq 0 ]; then
    echo ""
    echo "✅ Cluster force deleted via AWS CLI!"
    echo ""
    echo "⚠️  Note: You may need to manually clean up:"
    echo "    - VPC and subnets"
    echo "    - Security groups"
    echo "    - Terraform state files"
  fi
fi

echo ""
echo "✅ Cleanup complete!"
