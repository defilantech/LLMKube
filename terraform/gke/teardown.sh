#!/bin/bash
# Quick teardown script for GKE GPU cluster

set -e

echo "🗑️  LLMKube GKE GPU Cluster Teardown"
echo "===================================="
echo ""

# Check if terraform state exists
if [ ! -f terraform.tfstate ]; then
    echo "❌ No terraform state found. Cluster may not exist or was already destroyed."
    exit 1
fi

# Show current resources
echo "📊 Current cluster resources:"
terraform show | grep -E "(google_container|node_pool)" | head -20
echo ""

# Confirm
read -p "⚠️  Destroy cluster and all resources? This cannot be undone! (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

# Destroy
echo ""
echo "🔥 Destroying cluster..."
terraform destroy -auto-approve

# Verify
echo ""
echo "✅ Cluster destroyed successfully!"
echo ""
echo "💰 Cost savings: No more hourly charges!"
echo ""
echo "Note: If you had persistent volumes or load balancers, check GCP Console to ensure they're deleted."
