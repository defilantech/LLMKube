# EKS Cluster with Multi-GPU Support for LLMKube
# Based on AWS EKS best practices

terraform {
  required_version = ">= 1.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.20"
    }
  }
}

provider "aws" {
  region = var.region
}

# Data source for availability zones
data "aws_availability_zones" "available" {
  state = "available"
}

# VPC for EKS cluster
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.0"

  name = "${var.cluster_name}-vpc"
  cidr = "10.0.0.0/16"

  azs             = slice(data.aws_availability_zones.available.names, 0, 3)
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24", "10.0.103.0/24"]

  enable_nat_gateway   = true
  single_nat_gateway   = true
  enable_dns_hostnames = true
  enable_dns_support   = true

  # Tags required for EKS
  public_subnet_tags = {
    "kubernetes.io/role/elb" = "1"
  }

  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = "1"
  }

  tags = {
    Environment = "llmkube-test"
    ManagedBy   = "terraform"
  }
}

# EKS Cluster
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 19.0"

  cluster_name    = var.cluster_name
  cluster_version = "1.28"

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  # Cluster access
  cluster_endpoint_public_access = true

  # EKS Managed Node Groups
  eks_managed_node_groups = {
    # CPU node group for control plane workloads
    cpu = {
      name = "cpu-node-group"

      instance_types = ["t3.medium"]
      capacity_type  = var.use_spot ? "SPOT" : "ON_DEMAND"

      min_size     = 1
      max_size     = 3
      desired_size = 1

      labels = {
        role = "cpu"
      }

      tags = {
        NodeType = "cpu"
      }
    }

    # GPU node group for LLM inference
    gpu = {
      name = "gpu-node-group"

      # GPU instance types:
      # g4dn.xlarge:    1x T4 GPU, 4 vCPU, 16GB RAM (~$0.15/hr spot)
      # g4dn.2xlarge:   1x T4 GPU, 8 vCPU, 32GB RAM (~$0.23/hr spot)
      # g4dn.12xlarge:  4x T4 GPU, 48 vCPU, 192GB RAM (~$1.15/hr spot)
      # g5.xlarge:      1x A10G GPU, 4 vCPU, 16GB RAM (~$0.27/hr spot)
      # g5.2xlarge:     1x A10G GPU, 8 vCPU, 32GB RAM (~$0.41/hr spot)
      # g5.12xlarge:    4x A10G GPU, 48 vCPU, 192GB RAM (~$1.63/hr spot)
      instance_types = [var.gpu_instance_type]
      capacity_type  = var.use_spot ? "SPOT" : "ON_DEMAND"

      min_size     = var.min_gpu_nodes
      max_size     = var.max_gpu_nodes
      desired_size = var.min_gpu_nodes

      # Disk configuration
      block_device_mappings = {
        xvda = {
          device_name = "/dev/xvda"
          ebs = {
            volume_size           = var.disk_size_gb
            volume_type           = "gp3"
            delete_on_termination = true
          }
        }
      }

      # GPU taints to ensure only GPU workloads run on these nodes
      taints = [
        {
          key    = "nvidia.com/gpu"
          value  = "present"
          effect = "NO_SCHEDULE"
        }
      ]

      labels = {
        role                         = "gpu"
        "nvidia.com/gpu"             = "present"
        "k8s.amazonaws.com/accelerator" = var.gpu_type
      }

      tags = {
        NodeType = "gpu"
        GPUType  = var.gpu_type
        GPUCount = var.gpu_count
      }
    }
  }

  # AWS auth ConfigMap
  manage_aws_auth_configmap = true

  tags = {
    Environment = "llmkube-test"
    ManagedBy   = "terraform"
  }
}

# Install NVIDIA device plugin for GPU support
resource "null_resource" "install_nvidia_plugin" {
  depends_on = [module.eks]

  provisioner "local-exec" {
    command = <<-EOT
      aws eks update-kubeconfig --region ${var.region} --name ${var.cluster_name}
      kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.5/nvidia-device-plugin.yml

      # Wait for device plugin to be ready
      kubectl wait --for=condition=Ready pod -l name=nvidia-device-plugin-ds -n kube-system --timeout=120s || echo "Device plugin may still be starting..."
    EOT
  }
}
