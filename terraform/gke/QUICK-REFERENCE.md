# GKE GPU Cluster - Quick Reference

## 🚀 Create Cluster (One Command!)

```bash
cd terraform/gke
./quick-start.sh
```

This script will:
- Install gcloud CLI (if needed)
- Authenticate you with GCP
- Enable required APIs
- Create terraform.tfvars
- Create the cluster
- Configure kubectl

**Time**: ~5-10 minutes
**Cost**: ~$0.50/hr per GPU node (when running)

---

## 🗑️ Destroy Cluster (IMPORTANT!)

```bash
cd terraform/gke
./teardown.sh
```

**OR manually:**
```bash
terraform destroy
```

**Always do this when done testing to avoid charges!**

---

## 📊 Check Status

```bash
# Check if cluster exists
gcloud container clusters list

# Check nodes
kubectl get nodes

# Check GPU nodes specifically
kubectl get nodes -l role=gpu

# Check GPU availability
kubectl describe nodes | grep nvidia
```

---

## 💰 Cost Management

| Action | Cost Impact |
|--------|-------------|
| Cluster idle (0 GPU nodes) | ~$5/day (CPU nodes only) |
| 1 GPU node running | ~$12/day |
| Forgot to destroy | ~$360/month 😱 |

**Cost-Saving Tips:**
1. Set `min_gpu_nodes = 0` in terraform.tfvars (default)
2. Use `use_spot = true` (default, saves 70%)
3. Run `./teardown.sh` when done testing
4. Check GCP billing dashboard regularly

---

## 🧪 Test GPU Works

```bash
# Deploy GPU test pod
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda
    image: nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda11.7.1
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
EOF

# Wait for completion
kubectl wait --for=condition=complete pod/gpu-test --timeout=5m

# Check logs (should see "Test PASSED")
kubectl logs gpu-test

# Cleanup
kubectl delete pod gpu-test
```

---

## 🔧 Troubleshooting

**GPU nodes not appearing?**
```bash
# Check autoscaler
kubectl get pods -n kube-system | grep cluster-autoscaler

# Check events
kubectl get events --sort-by='.lastTimestamp'
```

**Out of quota?**
- Go to: https://console.cloud.google.com/iam-admin/quotas
- Filter: "GPUs (all regions)"
- Request increase

**Stuck on creation?**
```bash
# Check Terraform state
terraform show

# Check GCP console
gcloud container clusters describe llmkube-gpu-cluster --region=us-central1

# Force cleanup
terraform destroy -auto-approve
```

---

## 📦 Deploy LLMKube

```bash
# From project root
cd /Users/defilan/stuffy/code/ai/llmkube

# Install CRDs
kubectl apply -f config/crd/bases/

# Create a Model CR
kubectl apply -f config/samples/inference_v1alpha1_model.yaml

# Check status
kubectl get models
kubectl get mdl phi-3-mini -o yaml
```

---

## 🔐 Re-connecting Later

If you close your terminal and come back:

```bash
cd terraform/gke

# Get project and region from terraform.tfvars
PROJECT_ID=$(grep project_id terraform.tfvars | cut -d'"' -f2)
REGION=$(grep region terraform.tfvars | cut -d'"' -f2)

# Reconnect kubectl
gcloud container clusters get-credentials llmkube-gpu-cluster \
  --region=$REGION --project=$PROJECT_ID
```

---

## 📞 Support Links

- **GKE Pricing**: https://cloud.google.com/kubernetes-engine/pricing
- **GPU Pricing**: https://cloud.google.com/compute/gpus-pricing
- **Quota Requests**: https://console.cloud.google.com/iam-admin/quotas
- **GKE Docs**: https://cloud.google.com/kubernetes-engine/docs
- **GPU Docs**: https://cloud.google.com/kubernetes-engine/docs/how-to/gpus

---

## 🎯 Typical Workflow

```bash
# 1. Create cluster
cd terraform/gke && ./quick-start.sh

# 2. Test GPU
kubectl apply -f <gpu-test-pod>

# 3. Deploy llmkube
cd ../.. && kubectl apply -f config/crd/bases/

# 4. Test model
kubectl apply -f config/samples/inference_v1alpha1_model.yaml
kubectl logs -f <controller-pod>

# 5. CLEANUP!
cd terraform/gke && ./teardown.sh
```

---

**Remember: Always run `./teardown.sh` when done!** ⚠️
