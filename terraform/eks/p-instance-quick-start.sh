#!/bin/bash
# P Instance Quick Start Script for EKS
# Test multi-GPU NOW without waiting for G instance quota approval

set -e

echo "🚀 LLMKube P Instance (V100) Multi-GPU Test"
echo "============================================"
echo ""
echo "This will create a cluster with 4x V100 GPUs for immediate testing"
echo "Uses your existing P instance quota in us-west-2 (no waiting!)"
echo ""

# Check if AWS CLI is installed
if ! command -v aws &> /dev/null; then
    echo "❌ AWS CLI not found. Run ./aws-auth-setup.sh first"
    exit 1
fi

# Check if terraform is installed
if ! command -v terraform &> /dev/null; then
    echo "❌ Terraform not found. Run ./aws-auth-setup.sh first"
    exit 1
fi

# Check if kubectl is installed
if ! command -v kubectl &> /dev/null; then
    echo "❌ kubectl not found. Run ./aws-auth-setup.sh first"
    exit 1
fi

# Check if AWS credentials are configured
if ! aws sts get-caller-identity &> /dev/null; then
    echo "❌ AWS credentials not configured. Run ./aws-auth-setup.sh first"
    exit 1
fi

AWS_ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

echo "✅ Using AWS Account: $AWS_ACCOUNT"
echo "✅ Using Region: us-west-2"
echo ""

# Check P instance quota
echo "🔍 Checking P instance quota in us-west-2..."
P_QUOTA=$(aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-417A185B \
  --region us-west-2 \
  --query 'Quota.Value' \
  --output text 2>/dev/null || echo "0")

echo "P instance quota: $P_QUOTA vCPUs"
echo ""

if (( $(echo "$P_QUOTA < 32" | bc -l) )); then
    echo "❌ Insufficient P instance quota!"
    echo "   Need: 32 vCPUs (for p3.8xlarge)"
    echo "   Have: $P_QUOTA vCPUs"
    echo ""
    echo "Please request quota increase or try G instances in us-east-1"
    exit 1
fi

echo "✅ Quota check passed! You can run p3.8xlarge (4x V100 GPUs)"
echo ""

# Show configuration
echo "📋 Configuration Summary:"
echo "  AWS Account:  $AWS_ACCOUNT"
echo "  Region:       us-west-2"
echo "  Instance:     p3.8xlarge"
echo "  GPU Type:     V100"
echo "  GPUs/Node:    4"
echo "  Spot:         Yes (70% discount)"
echo ""
echo "💰 Cost Estimate:"
echo "  ~\$3.67/hr (spot pricing)"
echo "  ~\$7.34 for 2-hour test"
echo "  ~\$11.00 for 3-hour test"
echo ""

# Copy P instance config
echo "📝 Using P instance configuration..."
cp p-instance.tfvars terraform.tfvars
echo "✅ Configured for p3.8xlarge (4x V100)"

# Initialize Terraform if needed
if [ ! -d ".terraform" ]; then
    echo ""
    echo "🔧 Initializing Terraform..."
    terraform init
fi

# Show plan
echo ""
echo "📊 Terraform plan:"
terraform plan

# Confirm
echo ""
echo "⚠️  COST WARNING:"
echo "  - ~\$3.67/hr per GPU node (4x V100 spot)"
echo "  - ~\$7.34 for a 2-hour test session"
echo "  - Cluster auto-scales to 0 when idle (saves money)"
echo "  - REMEMBER: Run './teardown.sh' when done!"
echo ""
read -p "Create P instance cluster for testing? (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

# Apply
echo ""
echo "🏗️  Creating P instance cluster (this takes ~15-20 minutes)..."
terraform apply -auto-approve

# Get credentials
echo ""
echo "🔑 Configuring kubectl..."
eval $(terraform output -raw connect_command)

# Verify
echo ""
echo "✅ P instance cluster created successfully!"
echo ""
echo "📊 Cluster nodes:"
kubectl get nodes -o wide
echo ""
echo "🎮 GPU allocation:"
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.\"nvidia\\.com/gpu\"
echo ""

# Test GPU allocation
echo "🧪 Testing 4-GPU allocation with V100s..."
cat > /tmp/p-instance-test.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: v100-gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 4
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
EOF

kubectl apply -f /tmp/p-instance-test.yaml
echo "⏳ Waiting for GPU test pod (may take 2-3 min if scaling up node)..."
kubectl wait --for=condition=Ready pod/v100-gpu-test --timeout=300s || echo "Pod still pending (node may be starting)..."

if kubectl get pod v100-gpu-test -o jsonpath='{.status.phase}' | grep -q "Running\\|Succeeded"; then
    echo ""
    echo "✅ GPU Test Results:"
    kubectl logs v100-gpu-test
    echo ""
    echo "✅ SUCCESS: 4x V100 GPUs detected!"
else
    echo ""
    echo "⏳ Pod still pending. Check status with:"
    echo "   kubectl get pod v100-gpu-test"
    echo "   kubectl describe pod v100-gpu-test"
fi

kubectl delete pod v100-gpu-test 2>/dev/null || true
rm /tmp/p-instance-test.yaml

# Next steps
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ V100 Multi-GPU Cluster Ready!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "📚 Next Steps:"
echo ""
echo "1️⃣  Build and deploy multi-GPU controller:"
echo "   cd ../../"
echo "   export AWS_ACCOUNT_ID=\$(aws sts get-caller-identity --query Account --output text)"
echo "   export AWS_REGION=us-west-2"
echo "   export IMG=\${AWS_ACCOUNT_ID}.dkr.ecr.\${AWS_REGION}.amazonaws.com/llmkube-controller:v0.3.0-multi-gpu"
echo ""
echo "   # Create ECR repository"
echo "   aws ecr create-repository --repository-name llmkube-controller --region \$AWS_REGION || true"
echo "   aws ecr get-login-password --region \$AWS_REGION | docker login --username AWS --password-stdin \${AWS_ACCOUNT_ID}.dkr.ecr.\${AWS_REGION}.amazonaws.com"
echo ""
echo "   # Build and push (use --platform linux/amd64 for cross-platform)"
echo "   make docker-build IMG=\$IMG"
echo "   make docker-push IMG=\$IMG"
echo "   make install"
echo "   make deploy IMG=\$IMG"
echo ""
echo "2️⃣  Deploy multi-GPU test model (13B):"
echo "   kubectl apply -f config/samples/multi-gpu-llama-13b-model.yaml"
echo ""
echo "3️⃣  In parallel: Request G instance quota for cheaper long-term testing"
echo "   cd terraform/eks"
echo "   ./request-gpu-quota.sh"
echo ""
echo "💰 IMPORTANT: When done testing:"
echo "   cd terraform/eks"
echo "   ./teardown.sh"
echo ""
echo "📊 Monitor costs (should be ~\$3.67/hr when cluster is active):"
echo "   https://console.aws.amazon.com/cost-management/home"
echo ""
