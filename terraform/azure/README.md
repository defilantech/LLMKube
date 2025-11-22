# Azure AKS Multi-GPU Deployment

Deploy an Azure Kubernetes Service (AKS) cluster with multi-GPU support for testing LLMKube's multi-GPU inference capabilities.

## Overview

This Terraform configuration creates:
- AKS cluster in West US 2 (good GPU quota availability)
- System node pool (1-3 nodes, Standard_D2s_v3 for control plane)
- GPU node pool (0-2 nodes with 2 GPUs each, auto-scaling)
- NVIDIA device plugin for GPU scheduling
- Support for Azure Spot VMs (~80% cost savings)

## Prerequisites

### Required Tools
- [Azure CLI](https://docs.microsoft.com/en-us/cli/azure/install-azure-cli) (`az`)
- [Terraform](https://www.terraform.io/downloads) (v1.0+)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)

### Azure Requirements
- Active Azure subscription
- **GPU Quota**: Request quota increase for GPU VMs in West US 2
  - Portal: Subscriptions > Usage + quotas
  - Search for: "Standard NCv3 Family vCPUs" (for V100)
  - Request: At least 12 vCPUs for Standard_NC12s_v3 (2x V100)
  - Approval time: 1-2 business days

### Check Current Quota
```bash
# Login to Azure
az login

# Check quota for NCv3 (V100) in West US 2
az vm list-usage --location westus2 --query "[?contains(name.value, 'standardNCFamily')].{Name:name.localizedValue, Current:currentValue, Limit:limit}" --output table

# Request quota increase if needed:
# 1. Go to https://portal.azure.com
# 2. Navigate to: Subscriptions > Usage + quotas
# 3. Filter: Location = West US 2, Provider = Microsoft.Compute
# 4. Search: "Standard NCv3 Family"
# 5. Request increase to 12+ vCPUs
```

## Quick Start

### Option 1: Automated Script (Recommended)

```bash
cd terraform/azure
./multi-gpu-quick-start.sh
```

The script will:
1. Check prerequisites (Azure CLI, Terraform, kubectl)
2. Authenticate with Azure
3. Prompt for GPU type selection
4. Create configuration
5. Deploy the cluster
6. Install NVIDIA device plugin
7. Verify GPU setup

### Option 2: Manual Deployment

```bash
cd terraform/azure

# 1. Copy example config
cp terraform.tfvars.example terraform.tfvars

# 2. Edit terraform.tfvars and set your subscription ID
nano terraform.tfvars

# 3. Customize GPU configuration (see Configuration Options below)

# 4. Initialize Terraform
terraform init

# 5. Review plan
terraform plan

# 6. Deploy (takes ~10-15 minutes)
terraform apply

# 7. Get cluster credentials
eval $(terraform output -raw connect_command)

# 8. Verify cluster
kubectl get nodes
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds
```

## Configuration Options

### GPU VM Sizes

**Option 1: Standard_NC12s_v3 (Recommended for 2-GPU testing)**
```hcl
gpu_vm_size = "Standard_NC12s_v3"  # 2x V100 (16GB each)
gpu_count   = 2
```
- vCPUs: 12
- RAM: 112GB
- GPUs: 2x Tesla V100 (16GB VRAM each)
- Spot Price: ~$0.90/hr (~$650/mo if 24/7)
- On-Demand: ~$4.54/hr (~$3,270/mo if 24/7)

**Option 2: Standard_NC64as_T4_v3 (4x T4, use 2 of them)**
```hcl
gpu_vm_size = "Standard_NC64as_T4_v3"  # 4x T4 (16GB each)
gpu_count   = 2  # We'll use 2 of the 4 T4s
```
- vCPUs: 64
- RAM: 440GB
- GPUs: 4x Tesla T4 (16GB VRAM each, we'll use 2)
- Spot Price: ~$1.20/hr (~$865/mo if 24/7)
- On-Demand: ~$6.02/hr (~$4,340/mo if 24/7)

**Option 3: Standard_NC24ads_A100_v4 (Best Performance)**
```hcl
gpu_vm_size = "Standard_NC24ads_A100_v4"  # 2x A100 (80GB each)
gpu_count   = 2
```
- vCPUs: 24
- RAM: 220GB
- GPUs: 2x A100 (80GB VRAM each)
- Spot Price: ~$2.50/hr (~$1,800/mo if 24/7)
- On-Demand: ~$12.50/hr (~$9,000/mo if 24/7)

### Cost Optimization

**Auto-Scaling to Zero**
```hcl
min_gpu_nodes = 0  # Scale to 0 when idle
max_gpu_nodes = 2  # Scale up to 2 when needed
```
- Saves money when no workloads running
- Nodes auto-scale up when pods request GPUs
- Recommended for testing/development

**Azure Spot VMs**
```hcl
enable_spot    = true   # ~80% cheaper than on-demand
spot_max_price = -1     # Pay up to on-demand price (avoids eviction)
```
- Spot instances can be evicted but are much cheaper
- Setting `spot_max_price = -1` means you pay market price up to on-demand
- Good for testing, not recommended for production

### Region Selection

**West US 2 (Default - Recommended)**
- Good GPU quota availability
- Better success rate for GPU VM provisioning
- Competitive pricing

**Alternative Regions:**
```hcl
location = "eastus"      # East US
location = "southcentralus"  # South Central US
location = "westeurope"  # West Europe (if outside US)
```
**Note**: Check GPU availability in each region before changing.

## Verification

### Check Cluster is Running
```bash
# Get nodes
kubectl get nodes

# Check GPU allocation (may show 0 if auto-scaled to 0)
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
```

### Verify NVIDIA Device Plugin
```bash
# Check device plugin pods
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# View logs
kubectl logs -n kube-system -l name=nvidia-device-plugin-ds
```

### Test GPU Allocation
```bash
# Create test pod requesting 2 GPUs
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 2
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  restartPolicy: Never
EOF

# Wait for pod to complete (node will auto-scale up if needed)
kubectl wait --for=condition=complete pod/gpu-test --timeout=600s

# Check logs (should show 2 GPUs)
kubectl logs gpu-test

# Cleanup
kubectl delete pod gpu-test
```

Expected output should show 2 GPUs detected by `nvidia-smi`.

## Deploy Multi-GPU LLM Model

```bash
# Deploy 13B model with 2 GPUs
kubectl apply -f ../../config/samples/multi-gpu-llama-13b-model.yaml

# Watch deployment (GPU node will auto-scale up)
kubectl get inferenceservice -w

# Once ready, test inference
kubectl port-forward svc/llama-13b-multi-gpu-service 8080:8080 &

curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "max_tokens": 50
  }' | jq .
```

## Monitoring

### Watch GPU Nodes Scale
```bash
# Watch nodes
kubectl get nodes -w

# Watch GPU allocation
watch kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
```

### View GPU Metrics (if deployed)
```bash
# Port forward to Grafana
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Access at http://localhost:3000
# Default credentials: admin / prom-operator
```

## Troubleshooting

### GPU Nodes Not Starting
**Issue**: GPU node pool stuck at 0 nodes even when pod requests GPU.

**Solutions**:
```bash
# Check autoscaler status
kubectl describe configmap cluster-autoscaler-status -n kube-system

# Check for quota issues
az vm list-usage --location westus2 --query "[?contains(name.value, 'NCFamily')].{Name:name.localizedValue, Current:currentValue, Limit:limit}" --output table

# If quota is 0, request increase in Azure Portal
```

### NVIDIA Device Plugin Not Running
**Issue**: No NVIDIA device plugin pods in `kube-system` namespace.

**Solutions**:
```bash
# Manually install device plugin
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.0/nvidia-device-plugin.yml

# Verify installation
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# Check logs for errors
kubectl logs -n kube-system -l name=nvidia-device-plugin-ds
```

### Pod Stuck in Pending
**Issue**: Pod requesting GPUs stuck in Pending state.

**Solutions**:
```bash
# Describe pod to see reason
kubectl describe pod <pod-name>

# Common reasons:
# 1. No GPU nodes available (check autoscaling)
kubectl get nodes -l agentpool=gpupool

# 2. GPU quota exceeded
az vm list-usage --location westus2 --query "[?contains(name.value, 'NCFamily')]"

# 3. Taint not tolerated
# Make sure pod has toleration for nvidia.com/gpu=present:NoSchedule
```

### Spot Instance Evicted
**Issue**: Spot VM was evicted by Azure.

**Solutions**:
- Spot VMs can be evicted when Azure needs capacity
- Pod will reschedule when new spot capacity is available
- For production, use `enable_spot = false` (on-demand instances)
- Monitor eviction rate and adjust `spot_max_price` if needed

## Cost Management

### Estimated Costs (West US 2, Spot Pricing)

**With Auto-Scale to 0 (Recommended for Testing)**:
- System nodes: ~$70/mo (always running)
- GPU nodes: ~$0.90-2.50/hr when running
- **Total for testing**: ~$50-200/mo (assuming 2-4 hrs/day usage)

**24/7 Operation**:
- 2x V100 (Standard_NC12s_v3 spot): ~$650/mo per node
- 4x T4 (Standard_NC64as_T4_v3 spot): ~$865/mo per node
- 2x A100 (Standard_NC24ads_A100_v4 spot): ~$1,800/mo per node

### Cost-Saving Tips

1. **Auto-scale to 0**: Set `min_gpu_nodes = 0`
2. **Use Spot VMs**: Set `enable_spot = true` (~80% savings)
3. **Right-size VMs**: Start with V100 (cheaper), upgrade if needed
4. **Destroy when done**: Run `terraform destroy` after testing
5. **Set budget alerts**: Azure Portal > Cost Management > Budgets
6. **Monitor usage**: Check Azure Cost Management regularly

## Cleanup

### Destroy the Cluster

**IMPORTANT**: Always destroy the cluster when done testing to avoid charges!

```bash
cd terraform/azure

# Destroy all resources
terraform destroy

# Confirm when prompted
# This will delete: AKS cluster, resource group, all nodes
```

### Verify Deletion
```bash
# Check resource group is gone
az group show --name llmkube-multi-gpu-rg

# Should return: ResourceGroupNotFound
```

## Outputs

After successful deployment, Terraform outputs:

```bash
# View all outputs
terraform output

# Specific outputs
terraform output cluster_name
terraform output connect_command
terraform output estimated_hourly_cost
```

## Next Steps

1. **Deploy LLMKube Operator**: See main [README.md](../../README.md)
2. **Test Multi-GPU Model**: Follow [MULTI-GPU-DEPLOYMENT.md](../../MULTI-GPU-DEPLOYMENT.md)
3. **Run Benchmarks**: Use benchmark scripts in `test/e2e/`
4. **Monitor Performance**: Set up Grafana dashboards

## Support & Resources

- **LLMKube Docs**: [README.md](../../README.md)
- **Multi-GPU Guide**: [MULTI-GPU-DEPLOYMENT.md](../../MULTI-GPU-DEPLOYMENT.md)
- **Azure AKS Docs**: https://docs.microsoft.com/en-us/azure/aks/
- **Azure GPU VMs**: https://docs.microsoft.com/en-us/azure/virtual-machines/sizes-gpu
- **NVIDIA Device Plugin**: https://github.com/NVIDIA/k8s-device-plugin

## Contributing

Found an issue or have improvements? Please open an issue or PR on GitHub.

## License

Apache 2.0
