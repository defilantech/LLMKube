#!/bin/bash
# Teardown script for Azure AKS Multi-GPU cluster
# Safely destroys all resources to avoid ongoing costs

set -e

echo "🗑️  LLMKube Azure AKS Cluster Teardown"
echo "====================================="
echo ""

# Check if terraform is initialized
if [ ! -d ".terraform" ]; then
    echo "❌ Terraform not initialized in this directory"
    echo "Please run this script from terraform/azure directory"
    exit 1
fi

# Get cluster name from terraform state
CLUSTER_NAME=$(terraform output -raw cluster_name 2>/dev/null || echo "unknown")
RESOURCE_GROUP=$(terraform output -raw resource_group_name 2>/dev/null || echo "unknown")

echo "This will destroy:"
echo "  • AKS Cluster: $CLUSTER_NAME"
echo "  • Resource Group: $RESOURCE_GROUP"
echo "  • All GPU and system nodes"
echo "  • All associated resources (disks, IPs, etc.)"
echo ""
echo "⚠️  WARNING: This action cannot be undone!"
echo ""

read -p "Are you sure you want to destroy everything? (type 'yes' to confirm): " CONFIRM

if [ "$CONFIRM" != "yes" ]; then
    echo "Teardown cancelled."
    exit 0
fi

echo ""
echo "🔍 Checking for running workloads..."

# Try to check if there are any pods running (optional, may fail if cluster is already down)
if az aks get-credentials --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --overwrite-existing &>/dev/null; then
    RUNNING_PODS=$(kubectl get pods --all-namespaces --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l)
    if [ "$RUNNING_PODS" -gt 0 ]; then
        echo "⚠️  Found $RUNNING_PODS running pods"
        echo ""
        kubectl get pods --all-namespaces --field-selector=status.phase=Running
        echo ""
        read -p "Continue with teardown anyway? (y/n): " CONTINUE_ANYWAY
        if [[ ! "$CONTINUE_ANYWAY" =~ ^[Yy]$ ]]; then
            echo "Teardown cancelled."
            exit 0
        fi
    fi
fi

echo ""
echo "🗑️  Running terraform destroy..."
echo "This may take 5-10 minutes..."
echo ""

terraform destroy -auto-approve

echo ""
echo "✅ Verifying deletion..."

# Check if resource group still exists
if az group show --name "$RESOURCE_GROUP" &>/dev/null; then
    echo "⚠️  Resource group still exists. Manually deleting..."
    az group delete --name "$RESOURCE_GROUP" --yes --no-wait
    echo "   Deletion initiated (running in background)"
else
    echo "✅ Resource group deleted successfully"
fi

echo ""
echo "=================================================="
echo "✅ Teardown Complete!"
echo "=================================================="
echo ""
echo "All resources have been destroyed."
echo "You can verify in Azure Portal:"
echo "  https://portal.azure.com/#view/HubsExtension/BrowseResourceGroups"
echo ""
echo "💰 Billing will stop within a few hours."
echo ""
