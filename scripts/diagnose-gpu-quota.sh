#!/bin/bash
# Diagnose GPU quota and node pool issues

set -e

echo "🔍 GPU Quota and Node Pool Diagnostics"
echo "======================================="
echo ""

PROJECT_ID=$(gcloud config get-value project)
REGION="us-central1"
CLUSTER_NAME="llmkube-gpu-cluster"

echo "Project: $PROJECT_ID"
echo "Region: $REGION"
echo ""

# 1. Check current GPU quota
echo "1️⃣  Checking GPU quotas in region..."
echo ""
gcloud compute project-info describe --project=$PROJECT_ID \
  --format="table(quotas.metric,quotas.limit,quotas.usage)" \
  | grep -i "gpu\|NVIDIA" || echo "No GPU quotas found in project-level quotas"

echo ""
echo "Checking region-specific quotas for $REGION..."
gcloud compute regions describe $REGION \
  --format="table(quotas.metric,quotas.limit,quotas.usage)" \
  | grep -i "gpu\|NVIDIA" || echo "No GPU quotas found for region"

echo ""

# 2. Check GPU node pool status
echo "2️⃣  Checking GPU node pool configuration..."
echo ""
gcloud container node-pools describe gpu-pool \
  --cluster=$CLUSTER_NAME \
  --region=$REGION \
  --format="yaml(config.accelerators,autoscaling,instanceGroupUrls)" 2>/dev/null || \
  echo "⚠️  GPU node pool 'gpu-pool' not found or not accessible"

echo ""

# 3. Check for GPU VM instances
echo "3️⃣  Checking for GPU VM instances..."
echo ""
gcloud compute instances list \
  --filter="name~'gke-.*gpu.*'" \
  --format="table(name,zone,machineType,status,scheduling.preemptible)" || \
  echo "No GPU instances found"

echo ""

# 4. Check available GPU resources in zone
echo "4️⃣  Checking GPU availability in zones..."
echo ""
for ZONE in us-central1-a us-central1-b us-central1-c us-central1-f; do
  echo "Zone: $ZONE"
  gcloud compute accelerator-types list \
    --filter="zone:$ZONE AND name:nvidia-tesla-t4" \
    --format="table(name,zone)" 2>/dev/null || echo "  No T4 GPUs available"
done

echo ""

# 5. Provide quota increase instructions
echo "=========================================="
echo "📋 Diagnosis Summary"
echo "=========================================="
echo ""
echo "Common issues:"
echo ""
echo "1. **GPU Quota = 0 (New projects)**"
echo "   - New GCP projects have 0 GPU quota by default"
echo "   - You must request quota increase"
echo ""
echo "2. **Wrong region**"
echo "   - GPU quotas are region-specific"
echo "   - Current region: $REGION"
echo ""
echo "3. **Preemptible GPU quota**"
echo "   - Separate quota for preemptible/spot GPUs"
echo "   - Check: 'Preemptible NVIDIA T4 GPUs'"
echo ""
echo "=========================================="
echo "🔧 How to Fix"
echo "=========================================="
echo ""
echo "**Option 1: Request GPU Quota Increase (Recommended)**"
echo ""
echo "1. Go to: https://console.cloud.google.com/iam-admin/quotas?project=$PROJECT_ID"
echo ""
echo "2. Filter quotas:"
echo "   - Service: Compute Engine API"
echo "   - Search: 'NVIDIA T4 GPUs'"
echo "   - Locations: $REGION"
echo ""
echo "3. Select these quotas and click 'EDIT QUOTAS':"
echo "   ✓ NVIDIA T4 GPUs (all regions) → Request: 2"
echo "   ✓ NVIDIA T4 GPUs ($REGION) → Request: 2"
echo "   ✓ Preemptible NVIDIA T4 GPUs (all regions) → Request: 2"
echo "   ✓ Preemptible NVIDIA T4 GPUs ($REGION) → Request: 2"
echo ""
echo "4. Justification: 'LLM inference testing and development'"
echo ""
echo "5. Submit and wait (usually approved in minutes to hours)"
echo ""
echo "**Option 2: Use Different Region/Zone (Quick fix)**"
echo ""
echo "Some regions have better GPU availability:"
echo "  - us-west1 (Oregon)"
echo "  - us-east4 (Virginia)"
echo "  - europe-west4 (Netherlands)"
echo ""
echo "To change region:"
echo "  cd terraform/gke"
echo "  terraform destroy  # Clean up current cluster"
echo "  # Edit terraform.tfvars: region = 'us-west1'"
echo "  terraform apply"
echo ""
echo "**Option 3: Try Non-Preemptible (More expensive)**"
echo ""
echo "Edit terraform/gke/terraform.tfvars:"
echo "  use_spot = false  # Use on-demand instead of spot"
echo ""
echo "Then: terraform apply"
echo ""
echo "=========================================="
echo "Quick Commands"
echo "=========================================="
echo ""
echo "# Check quota increase status"
echo "gcloud compute project-info describe --project=$PROJECT_ID"
echo ""
echo "# List all GPU quotas"
echo "gcloud compute regions describe $REGION | grep -i gpu"
echo ""
echo "# Try manual GPU node pool scale"
echo "gcloud container clusters resize $CLUSTER_NAME \\"
echo "  --region=$REGION --node-pool=gpu-pool --num-nodes=1"
echo ""
