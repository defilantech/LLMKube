# EKS GPU Cluster for LLMKube

Terraform configuration for a cost-optimized EKS cluster with GPU support for running LLMKube on AWS.

## Why EKS?

- **Better default GPU quotas**: AWS typically provides 4-8 GPUs by default (vs 1 GPU on GCP)
- **Multi-GPU instances**: g4dn.12xlarge has 4x T4 GPUs, g5.12xlarge has 4x A10G GPUs
- **Good spot pricing**: ~70% discount on GPU instances
- **Wide availability**: GPU instances available in most regions

## Prerequisites

1. **Install AWS CLI**:
   ```bash
   # macOS
   brew install awscli

   # Configure credentials
   aws configure
   ```

2. **Install Terraform**:
   ```bash
   brew install terraform
   ```

3. **Install kubectl**:
   ```bash
   brew install kubectl
   ```

4. **AWS Account Setup**:
   - Ensure you have an AWS account with billing enabled
   - Check GPU quota limits (default is usually 4+ GPUs)
   - Recommended regions: us-east-1, us-west-2 (best GPU availability)

## Quick Start

### Option 1: Multi-GPU Quick Start (Recommended)

```bash
# Use the automated script
./multi-gpu-quick-start.sh
```

This will:
- Check prerequisites and install missing tools
- Configure AWS credentials
- Prompt for GPU type selection (4x T4 or 4x A10G)
- Create terraform.tfvars with multi-GPU config
- Deploy EKS cluster (~15-20 minutes)
- Test GPU allocation

### Option 2: Manual Configuration

1. **Copy example config**:
   ```bash
   cp terraform.tfvars.example terraform.tfvars
   ```

2. **Edit `terraform.tfvars`** and choose your configuration

3. **Deploy cluster**:
   ```bash
   # Initialize Terraform
   terraform init

   # Review plan
   terraform plan

   # Create cluster (takes ~15-20 minutes)
   terraform apply

   # Get kubectl credentials
   eval $(terraform output -raw connect_command)

   # Verify cluster
   kubectl get nodes
   kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.\"nvidia\\.com/gpu\"
   ```

## GPU Instance Types

### T4 GPUs (Cost-effective)
| Instance Type | GPUs | vCPUs | RAM | Spot Price/hr | Use Case |
|--------------|------|-------|-----|---------------|----------|
| g4dn.xlarge | 1x T4 | 4 | 16GB | ~$0.15 | Single GPU testing |
| g4dn.2xlarge | 1x T4 | 8 | 32GB | ~$0.23 | Single GPU production |
| g4dn.12xlarge | 4x T4 | 48 | 192GB | ~$1.15 | **Multi-GPU testing** |

### A10G GPUs (Better performance)
| Instance Type | GPUs | vCPUs | RAM | Spot Price/hr | Use Case |
|--------------|------|-------|-----|---------------|----------|
| g5.xlarge | 1x A10G | 4 | 16GB | ~$0.27 | Single GPU testing |
| g5.2xlarge | 1x A10G | 8 | 32GB | ~$0.41 | Single GPU production |
| g5.12xlarge | 4x A10G | 48 | 192GB | ~$1.63 | **Multi-GPU production** |

## Configuration

### Single-GPU Setup (Default)

```hcl
region            = "us-east-1"
cluster_name      = "llmkube-gpu-cluster"
gpu_instance_type = "g4dn.xlarge"  # 1x T4 GPU
gpu_type          = "tesla-t4"
gpu_count         = 1
min_gpu_nodes     = 0  # Auto-scale to 0 when idle
max_gpu_nodes     = 2
use_spot          = true  # 70% discount
```

### Multi-GPU Setup (4x GPUs)

**Option A: 4x T4 GPUs** (Recommended for testing)
```hcl
gpu_instance_type = "g4dn.12xlarge"
gpu_type          = "tesla-t4"
gpu_count         = 4
disk_size_gb      = 200
```

**Option B: 4x A10G GPUs** (Better performance)
```hcl
gpu_instance_type = "g5.12xlarge"
gpu_type          = "nvidia-a10g"
gpu_count         = 4
disk_size_gb      = 200
```

## Testing GPU

### Single GPU Test

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
EOF

# Check logs (should show 1 GPU)
kubectl logs gpu-test

# Clean up
kubectl delete pod gpu-test
```

### Multi-GPU Test (4 GPUs)

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: multi-gpu-test
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

# Wait for pod (may take 2-3 min if node needs to scale up)
kubectl wait --for=condition=Ready pod/multi-gpu-test --timeout=300s

# Check logs (should show 4 GPUs)
kubectl logs multi-gpu-test

# Clean up
kubectl delete pod multi-gpu-test
```

## Deploying LLMKube

```bash
# From llmkube project root
cd /Users/defilan/stuffy/code/ai/llmkube

# Get AWS account ID and region
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export AWS_REGION=$(aws configure get region)

# Create ECR repository for controller image
aws ecr create-repository --repository-name llmkube-controller --region $AWS_REGION || true

# Authenticate Docker with ECR
aws ecr get-login-password --region $AWS_REGION | \
  docker login --username AWS --password-stdin ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com

# Set image name
export IMG=${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/llmkube-controller:v0.3.0-multi-gpu

# Build and push controller
make docker-build IMG=$IMG
make docker-push IMG=$IMG

# Install CRDs and deploy controller
make install
make deploy IMG=$IMG

# Verify controller is running
kubectl get pods -n llmkube-system
```

## Cost Optimization

**Current Configuration:**
- **Min GPU nodes**: 0 (saves money when not testing)
- **Max GPU nodes**: 2 (limit spending)
- **Spot instances**: Enabled (~70% discount)
- **CPU nodes**: t3.medium (cheap for control plane)

**Estimated Costs (us-east-1, Spot pricing):**
| Scenario | Cost |
|----------|------|
| Idle (0 GPU nodes) | ~$3/day (CPU nodes + EKS control plane) |
| 1 GPU node (g4dn.12xlarge) | ~$30/day (~$1.15/hr) |
| Left running by accident | ~$900/month |

**To minimize costs:**
1. Run `terraform destroy` when done testing
2. Set `min_gpu_nodes = 0` (default)
3. Use `use_spot = true` (default)
4. Delete cluster after major milestones

## Tear Down

### Option A: Destroy Entire Cluster (Recommended)

```bash
# Simple teardown
./teardown.sh

# Or directly with terraform
terraform destroy
```

### Option B: Force Cleanup (if normal destroy fails)

```bash
# Aggressive cleanup that removes all resources first
./force-cleanup.sh
```

### Option C: Keep Cluster, Remove Workloads

```bash
# Delete inference services and models
kubectl delete inferenceservice --all
kubectl delete model --all

# Cluster will auto-scale GPU nodes to 0 (if min_gpu_nodes=0)
```

### Verify Cleanup

```bash
# Check if cluster is deleted
aws eks list-clusters --region us-east-1

# Check for orphaned resources
aws ec2 describe-instances --region us-east-1 \
  --filters "Name=tag:eks:cluster-name,Values=llmkube-*" \
  --query "Reservations[].Instances[].InstanceId"
```

## Troubleshooting

### GPU nodes not scaling up?

```bash
# Check node group status
aws eks describe-nodegroup \
  --cluster-name llmkube-multi-gpu-test \
  --nodegroup-name gpu-node-group \
  --region us-east-1

# Check for pending pods
kubectl get pods -A | grep Pending

# Check cluster autoscaler logs
kubectl logs -n kube-system deployment/cluster-autoscaler
```

### "Quota exceeded" error?

```bash
# Check current GPU quota
aws service-quotas get-service-quota \
  --service-code ec2 \
  --quota-code L-3819A6DF \
  --region us-east-1

# Request quota increase
aws service-quotas request-service-quota-increase \
  --service-code ec2 \
  --quota-code L-3819A6DF \
  --desired-value 8 \
  --region us-east-1
```

### Cluster stuck creating?

```bash
# Check cluster status
aws eks describe-cluster \
  --name llmkube-multi-gpu-test \
  --region us-east-1 \
  --query "cluster.status"

# View CloudFormation events
aws cloudformation describe-stack-events \
  --stack-name eksctl-llmkube-multi-gpu-test-cluster \
  --region us-east-1
```

### NVIDIA device plugin not working?

```bash
# Check device plugin pods
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# Reinstall device plugin
kubectl delete -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.5/nvidia-device-plugin.yml
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.5/nvidia-device-plugin.yml
```

## Comparison: EKS vs GKE

| Feature | EKS | GKE |
|---------|-----|-----|
| Default GPU Quota | 4-8 GPUs | 1 GPU |
| Multi-GPU Instance | g4dn.12xlarge (4x T4) | n1-standard-8 + 2x T4 |
| Spot Discount | ~70% | ~70% |
| Setup Time | ~15-20 min | ~10-15 min |
| GPU Availability | Excellent | Good (zone restrictions) |
| Cost (4x T4 spot) | ~$1.15/hr | ~$0.70/hr (2x T4 only) |

**EKS Advantages:**
- ✅ Better default GPU quotas (no wait for approval)
- ✅ True 4-GPU instances (g4dn.12xlarge, g5.12xlarge)
- ✅ Wider GPU availability across regions

**GKE Advantages:**
- ✅ Slightly cheaper for 2-GPU config
- ✅ Faster cluster creation
- ✅ Better regional load balancing

## Cleanup Checklist

Before closing your session:
- [ ] Run `./teardown.sh` or `terraform destroy`
- [ ] Verify cluster deleted: `aws eks list-clusters`
- [ ] Check for orphaned EBS volumes: `aws ec2 describe-volumes`
- [ ] Check for orphaned load balancers: `aws elb describe-load-balancers`
- [ ] Verify billing to ensure no unexpected charges

## Support

If you encounter issues:

1. Check the [troubleshooting section](#troubleshooting) above
2. Review full test plan: `/Users/defilan/stuffy/code/ai/llmkube/test/e2e/multi-gpu-test-plan.md`
3. File issue with logs: `kubectl logs <pod> > pod-logs.txt`
4. Include GPU info: `kubectl exec <pod> -- nvidia-smi > gpu-info.txt`
