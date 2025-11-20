#!/bin/bash
# Request GPU quota increase for AWS EKS multi-GPU testing

set -e

echo "📊 AWS GPU Quota Increase Request"
echo "=================================="
echo ""

# Get current region
AWS_REGION=$(aws configure get region || echo "us-east-1")

echo "Current region: $AWS_REGION"
echo ""

# Check current quota
echo "🔍 Checking current GPU quota..."
CURRENT_QUOTA=$(aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-3819A6DF \
  --region $AWS_REGION \
  --query 'Quota.Value' \
  --output text 2>/dev/null || echo "0.0")

echo "Current quota (Running On-Demand G instances): $CURRENT_QUOTA vCPUs"
echo ""

# Ask user what they need
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "What do you want to test?"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1) Single GPU (g4dn.xlarge - 1x T4)      → Request 4 vCPUs"
echo "2) Multi-GPU (g4dn.12xlarge - 4x T4)     → Request 48 vCPUs  ⭐ Recommended"
echo "3) Multi-GPU HA (2x g4dn.12xlarge)       → Request 96 vCPUs"
echo "4) Multi-GPU A10G (g5.12xlarge - 4x A10G)→ Request 48 vCPUs"
echo "5) Custom value"
echo ""
read -p "Choice (1-5): " CHOICE

case $CHOICE in
  1)
    REQUESTED_VCPUS=4
    DESCRIPTION="Single GPU testing with g4dn.xlarge (1x T4 GPU)"
    ;;
  2)
    REQUESTED_VCPUS=48
    DESCRIPTION="Multi-GPU testing with g4dn.12xlarge (4x T4 GPUs)"
    ;;
  3)
    REQUESTED_VCPUS=96
    DESCRIPTION="Multi-GPU HA testing with 2x g4dn.12xlarge (8x T4 GPUs total)"
    ;;
  4)
    REQUESTED_VCPUS=48
    DESCRIPTION="Multi-GPU testing with g5.12xlarge (4x A10G GPUs)"
    ;;
  5)
    read -p "Enter desired vCPU count: " REQUESTED_VCPUS
    read -p "Enter description: " DESCRIPTION
    ;;
  *)
    echo "Invalid choice"
    exit 1
    ;;
esac

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Request Summary"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Service:       EC2"
echo "Quota:         Running On-Demand G instances"
echo "Region:        $AWS_REGION"
echo "Current:       $CURRENT_QUOTA vCPUs"
echo "Requested:     $REQUESTED_VCPUS vCPUs"
echo "Description:   $DESCRIPTION"
echo ""
read -p "Submit request? (y/N): " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cancelled."
    exit 0
fi

echo ""
echo "📤 Submitting quota increase request..."
echo ""

FULL_DESCRIPTION="LLMKube multi-GPU testing - Open source Kubernetes operator for LLM inference. $DESCRIPTION. Testing layer-based multi-GPU sharding for Llama models."

RESULT=$(aws service-quotas request-service-quota-increase \
  --service-code ec2 \
  --quota-code L-3819A6DF \
  --desired-value $REQUESTED_VCPUS \
  --region $AWS_REGION \
  --output json 2>&1)

if [ $? -eq 0 ]; then
    REQUEST_ID=$(echo "$RESULT" | jq -r '.RequestedQuota.Id')
    REQUEST_STATUS=$(echo "$RESULT" | jq -r '.RequestedQuota.Status')

    echo "✅ Quota increase request submitted!"
    echo ""
    echo "Request ID: $REQUEST_ID"
    echo "Status:     $REQUEST_STATUS"
    echo ""

    if [[ "$REQUEST_STATUS" == "APPROVED" || "$REQUEST_STATUS" == "CASE_OPENED" ]]; then
        echo "🎉 Request was approved or is being processed!"
        echo ""
        echo "Next steps:"
        echo "  1. Wait a few minutes for changes to propagate"
        echo "  2. Run ./aws-auth-setup.sh to verify new quota"
        echo "  3. Deploy cluster: ./multi-gpu-quick-start.sh"
    else
        echo "📋 Request submitted and pending review."
        echo ""
        echo "Typical approval time:"
        echo "  • Instant (for common requests like 48-96 vCPUs)"
        echo "  • 1-2 business days (for larger requests)"
        echo ""
        echo "Check status:"
        echo "  aws service-quotas list-requested-service-quota-change-history-by-quota \\"
        echo "    --service-code ec2 \\"
        echo "    --quota-code L-3819A6DF \\"
        echo "    --region $AWS_REGION"
        echo ""
        echo "Or check in AWS Console:"
        echo "  https://console.aws.amazon.com/servicequotas/home/requests"
    fi
else
    echo "❌ Request failed:"
    echo "$RESULT"
    echo ""
    echo "Common issues:"
    echo "  • Account too new (wait 24-48 hours after account creation)"
    echo "  • Previous request pending (check existing requests)"
    echo "  • IAM permissions missing (need servicequotas:RequestServiceQuotaIncrease)"
    echo ""
    echo "Alternative: Request via AWS Console:"
    echo "  https://console.aws.amazon.com/servicequotas/home/services/ec2/quotas/L-3819A6DF"
fi

echo ""
