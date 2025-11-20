# GKE GPU Cluster for LLMKube

Terraform configuration for a cost-optimized GKE cluster with GPU support for running LLMKube.

## Prerequisites

1. **Install gcloud CLI**:
   ```bash
   # macOS
   brew install --cask google-cloud-sdk

   # Authenticate
   gcloud auth login
   gcloud auth application-default login
   ```

2. **Create/Select GCP Project**:
   ```bash
   # List projects
   gcloud projects list

   # Create new project (if needed)
   gcloud projects create llmkube-demo --name="LLMKube Demo"

   # Set project
   gcloud config set project YOUR_PROJECT_ID
   ```

3. **Enable Required APIs**:
   ```bash
   gcloud services enable container.googleapis.com
   gcloud services enable compute.googleapis.com
   ```

4. **Install Terraform**:
   ```bash
   brew install terraform
   ```

## Configuration

### Single-GPU Setup (Default)

1. **Copy example config**:
   ```bash
   cp terraform.tfvars.example terraform.tfvars
   ```

2. **Edit `terraform.tfvars`**:
   ```hcl
   project_id = "your-gcp-project-id"  # REQUIRED: Your GCP project
   region     = "us-central1"           # Change if needed
   use_spot   = true                    # true = cheap but interruptible
   ```

### Multi-GPU Setup (For Issue #2 Testing)

For testing multi-GPU single-node support:

1. **Use multi-GPU config**:
   ```bash
   cp multi-gpu.tfvars terraform.tfvars
   ```

2. **Edit `terraform.tfvars`** and set your `project_id`

3. **Choose GPU configuration**:

   **Option A: 2x T4 GPUs** (Recommended for testing)
   ```hcl
   gpu_type     = "nvidia-tesla-t4"
   gpu_count    = 2
   machine_type = "n1-standard-8"
   ```
   - Cost: ~$0.70/hr per node (spot)
   - Good for: 13B models, cost-effective testing

   **Option B: 2x L4 GPUs** (Better performance)
   ```hcl
   gpu_type     = "nvidia-l4"
   gpu_count    = 2
   machine_type = "g2-standard-24"
   ```
   - Cost: ~$1.40/hr per node (spot)
   - Good for: 13B-70B models, production testing

## Usage

### Create Cluster

```bash
# Initialize Terraform
terraform init

# Preview changes
terraform plan

# Create cluster (takes ~5-10 minutes)
terraform apply

# Get kubectl credentials
eval $(terraform output -raw connect_command)

# Verify cluster
kubectl get nodes
kubectl describe nodes | grep nvidia  # Check GPU
```

### Tear Down (IMPORTANT for cost savings!)

```bash
# Destroy entire cluster
terraform destroy

# Or just scale GPU nodes to 0
kubectl scale deployment --all --replicas=0 -n default
```

## Cost Optimization

**Current Configuration:**
- **Min GPU nodes**: 0 (saves money when not testing)
- **Max GPU nodes**: 2 (limit spending)
- **Spot instances**: Enabled (~70% discount)
- **CPU pool**: e2-medium (cheap for control plane)

**Estimated Costs:**
| Scenario | Cost |
|----------|------|
| Idle (0 GPU nodes) | ~$5/day (CPU nodes only) |
| 1 GPU node running | ~$12/day (~$0.50/hr GPU) |
| Left running by accident | ~$360/month |

**To minimize costs:**
1. Run `terraform destroy` when done testing
2. Set `min_gpu_nodes = 0` (default)
3. Use `use_spot = true` (default)
4. Delete cluster after major milestones

## Testing GPU

### Single GPU Test

After cluster creation:

```bash
# Run GPU test pod
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-container
    image: nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda11.7.1
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
EOF

# Check logs (should show "Test PASSED")
kubectl logs gpu-test

# Clean up
kubectl delete pod gpu-test
```

### Multi-GPU Test

For clusters with 2+ GPUs per node:

```bash
# Check GPU availability
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
# Should show: 2 (or more) per GPU node

# Test 2-GPU allocation
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: multi-gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-container
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 2
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  nodeSelector:
    cloud.google.com/gke-nodepool: gpu-pool
EOF

# Check logs (should show 2 GPUs)
kubectl logs multi-gpu-test
# Expected: GPU 0 and GPU 1 listed

# Clean up
kubectl delete pod multi-gpu-test
```

## Deploying LLMKube

```bash
# From llmkube project root
cd /Users/defilan/stuffy/code/ai/llmkube

# Install CRDs
kubectl apply -f config/crd/bases/

# Deploy operator (when ready)
make docker-build IMG=gcr.io/${PROJECT_ID}/llmkube:latest
make docker-push IMG=gcr.io/${PROJECT_ID}/llmkube:latest
make deploy IMG=gcr.io/${PROJECT_ID}/llmkube:latest
```

## Troubleshooting

**GPU nodes not scaling up?**
```bash
# Check node pool status
kubectl get nodes -l role=gpu

# Check for pending pods
kubectl get pods -A | grep Pending

# Check autoscaler logs
kubectl logs -n kube-system -l k8s-app=cluster-autoscaler
```

**"Quota exceeded" error?**
- Request GPU quota increase: https://console.cloud.google.com/iam-admin/quotas
- Or choose a different region with availability

**Cluster stuck creating?**
```bash
# Check GKE status
gcloud container clusters describe llmkube-gpu-cluster --region=us-central1

# View events
gcloud logging read "resource.type=gke_cluster" --limit 50
```

## Cleanup Checklist

Before closing your laptop:
- [ ] Run `terraform destroy`
- [ ] Verify in GCP Console that cluster is deleted
- [ ] Check billing to ensure no unexpected charges
