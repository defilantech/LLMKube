#!/bin/bash
# Quick start script for GKE GPU cluster

set -e

echo "🚀 LLMKube GKE GPU Cluster Setup"
echo "================================"
echo ""

# Check if gcloud is installed
if ! command -v gcloud &> /dev/null; then
    echo "❌ gcloud CLI not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        brew install --cask google-cloud-sdk
    else
        echo "Please install gcloud CLI: https://cloud.google.com/sdk/docs/install"
        exit 1
    fi
fi

# Check if terraform is installed
if ! command -v terraform &> /dev/null; then
    echo "❌ Terraform not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        brew install terraform
    else
        echo "Please install Terraform: https://www.terraform.io/downloads"
        exit 1
    fi
fi

# Install GKE auth plugin if not present
if ! command -v gke-gcloud-auth-plugin &> /dev/null; then
    echo "📦 Installing GKE auth plugin..."
    gcloud components install gke-gcloud-auth-plugin --quiet
fi

# Check if authenticated
if ! gcloud auth list --filter=status:ACTIVE --format="value(account)" | grep -q "."; then
    echo "🔐 Authenticating with GCP..."
    gcloud auth login
    gcloud auth application-default login
fi

# Get or set project
CURRENT_PROJECT=$(gcloud config get-value project 2>/dev/null)
if [ -z "$CURRENT_PROJECT" ]; then
    echo ""
    echo "📋 Available projects:"
    gcloud projects list
    echo ""
    read -p "Enter project ID (or press enter to create new): " PROJECT_ID

    if [ -z "$PROJECT_ID" ]; then
        read -p "Enter new project ID: " PROJECT_ID
        echo "Creating project $PROJECT_ID..."
        gcloud projects create "$PROJECT_ID" --name="LLMKube Demo"
    fi

    gcloud config set project "$PROJECT_ID"
else
    PROJECT_ID=$CURRENT_PROJECT
    echo "✅ Using project: $PROJECT_ID"
fi

# Enable required APIs
echo ""
echo "🔌 Enabling required GCP APIs..."
gcloud services enable container.googleapis.com --project="$PROJECT_ID" 2>/dev/null || true
gcloud services enable compute.googleapis.com --project="$PROJECT_ID" 2>/dev/null || true

# Create terraform.tfvars if it doesn't exist
if [ ! -f terraform.tfvars ]; then
    echo ""
    echo "📝 Creating terraform.tfvars..."
    cat > terraform.tfvars <<EOF
project_id   = "$PROJECT_ID"
region       = "us-central1"
cluster_name = "llmkube-gpu-cluster"

# GPU Configuration
gpu_type     = "nvidia-tesla-t4"
gpu_count    = 1
machine_type = "n1-standard-4"

# Auto-scaling (start at 0 to save money)
min_gpu_nodes = 0
max_gpu_nodes = 2

# Cost savings
use_spot = true

# Storage
disk_size_gb = 100
EOF
    echo "✅ Created terraform.tfvars"
fi

# Initialize Terraform
echo ""
echo "🔧 Initializing Terraform..."
terraform init

# Show plan
echo ""
echo "📊 Terraform plan:"
terraform plan

# Confirm
echo ""
read -p "Create cluster? This will cost ~$0.50/hr per GPU node. (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

# Apply
echo ""
echo "🏗️  Creating cluster (this takes ~5-10 minutes)..."
terraform apply -auto-approve

# Get credentials
echo ""
echo "🔑 Configuring kubectl..."
eval $(terraform output -raw connect_command)

# Verify
echo ""
echo "✅ Cluster created successfully!"
echo ""
kubectl get nodes
echo ""
echo "📊 GPU node pool status:"
kubectl get nodes -l role=gpu -o wide
echo ""
echo "💰 Remember to run 'terraform destroy' when done to avoid charges!"
echo ""
echo "Next steps:"
echo "  1. Test GPU: kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/container-engine-accelerators/master/nvidia-driver-installer/cos/daemonset-preloaded.yaml"
echo "  2. Deploy llmkube: cd ../../ && kubectl apply -f config/crd/bases/"
echo "  3. Create a Model CR to test GPU inference"
