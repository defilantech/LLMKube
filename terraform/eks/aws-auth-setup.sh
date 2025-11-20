#!/bin/bash
# AWS authentication setup for LLMKube
# This script configures AWS CLI credentials and Docker authentication for ECR

set -e

echo "🔐 AWS CLI Authentication Setup"
echo "================================"
echo ""

# Check if AWS CLI is installed
if ! command -v aws &> /dev/null; then
    echo "❌ AWS CLI not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        brew install awscli
    else
        echo "Please install AWS CLI: https://aws.amazon.com/cli/"
        echo ""
        echo "Installation instructions:"
        echo "  Linux:   curl 'https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip' -o 'awscliv2.zip' && unzip awscliv2.zip && sudo ./aws/install"
        echo "  macOS:   brew install awscli"
        echo "  Windows: Download from https://aws.amazon.com/cli/"
        exit 1
    fi
fi

echo "✅ AWS CLI version: $(aws --version)"
echo ""

# Check if already configured
if aws sts get-caller-identity &> /dev/null 2>&1; then
    echo "✅ AWS credentials already configured!"
    echo ""
    echo "Current identity:"
    aws sts get-caller-identity
    echo ""
    read -p "Reconfigure? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Keeping existing configuration."
        SKIP_CONFIGURE=true
    fi
fi

if [ "$SKIP_CONFIGURE" != "true" ]; then
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "AWS Credentials Setup"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "You'll need:"
    echo "  1. AWS Access Key ID (from AWS Console → IAM → Security credentials)"
    echo "  2. AWS Secret Access Key"
    echo "  3. Default region (recommend: us-east-1 for best GPU availability)"
    echo ""
    echo "To get credentials:"
    echo "  1. Go to: https://console.aws.amazon.com/iam/"
    echo "  2. IAM → Users → Add users (or use existing user)"
    echo "  3. Enable 'Programmatic access'"
    echo "  4. Attach policies: AdministratorAccess (or EKS/EC2 specific policies)"
    echo "  5. Save the Access Key ID and Secret Access Key"
    echo ""
    read -p "Press Enter to continue with aws configure..."
    echo ""

    aws configure
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Verifying Authentication"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if ! aws sts get-caller-identity; then
    echo ""
    echo "❌ Authentication failed. Please check your credentials."
    echo ""
    echo "Common issues:"
    echo "  - Invalid Access Key ID or Secret Access Key"
    echo "  - IAM user doesn't have sufficient permissions"
    echo "  - Network connectivity issues"
    echo ""
    exit 1
fi

echo ""
echo "✅ AWS CLI configured successfully!"

# Get account info
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
AWS_REGION=$(aws configure get region)

if [ -z "$AWS_REGION" ]; then
    echo "⚠️  No default region set. Setting to us-east-1..."
    aws configure set region us-east-1
    AWS_REGION="us-east-1"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Setting up Docker Authentication for ECR"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Check if Docker is running
if ! docker info &> /dev/null; then
    echo "⚠️  Docker is not running. Please start Docker Desktop and run this script again."
    echo ""
    echo "Current configuration:"
    echo "  AWS Account: $AWS_ACCOUNT_ID"
    echo "  Region:      $AWS_REGION"
    echo ""
    echo "To authenticate Docker later, run:"
    echo "  aws ecr get-login-password --region $AWS_REGION | \\"
    echo "    docker login --username AWS --password-stdin ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com"
    exit 0
fi

echo "🐳 Authenticating Docker with ECR..."
if aws ecr get-login-password --region $AWS_REGION | \
   docker login --username AWS --password-stdin ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com; then
    echo ""
    echo "✅ Docker authentication successful!"
else
    echo ""
    echo "⚠️  Docker authentication failed. You may need to:"
    echo "  1. Ensure your IAM user has ECR permissions"
    echo "  2. Check that Docker is running"
    echo ""
    echo "To retry later:"
    echo "  aws ecr get-login-password --region $AWS_REGION | \\"
    echo "    docker login --username AWS --password-stdin ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Setup Complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "AWS Account: $AWS_ACCOUNT_ID"
echo "Region:      $AWS_REGION"
echo ""

# Check GPU quota
echo "🎮 Checking GPU quota..."
GPU_QUOTA=$(aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-3819A6DF \
  --region $AWS_REGION \
  --query 'Quota.Value' \
  --output text 2>/dev/null || echo "0")

if [ "$GPU_QUOTA" != "0" ]; then
    echo "✅ GPU Quota (Running On-Demand G instances): $GPU_QUOTA"

    if (( $(echo "$GPU_QUOTA >= 4" | bc -l) )); then
        echo "   Great! You have enough quota for multi-GPU testing (4+ GPUs)"
    else
        echo "   ⚠️  You have $GPU_QUOTA GPUs available. Multi-GPU instances need 4+ GPUs."
        echo "   You can still test with single-GPU instances or request quota increase."
    fi
else
    echo "⚠️  Could not check GPU quota (may need permissions)"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Next Steps"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1️⃣  Deploy multi-GPU EKS cluster:"
echo "   ./multi-gpu-quick-start.sh"
echo ""
echo "2️⃣  Or manually with Terraform:"
echo "   terraform init"
echo "   terraform apply"
echo ""
echo "3️⃣  Check available resources:"
echo "   aws ec2 describe-instance-types --filters 'Name=instance-type,Values=g4dn.*' --region $AWS_REGION"
echo "   aws ec2 describe-instance-types --filters 'Name=instance-type,Values=g5.*' --region $AWS_REGION"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
