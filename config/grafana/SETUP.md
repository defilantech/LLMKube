# LLMKube GPU Monitoring Setup

This guide covers setting up the monitoring stack for the LLMKube GPU dashboard.

## Prerequisites

- Prometheus server running
- Grafana instance
- NVIDIA GPU(s) with drivers installed
- Docker (for running exporters)

## 1. Install Node Exporter (System Metrics)

Node exporter provides CPU, memory, disk, network, and temperature metrics.

### Option A: Docker (Recommended)

```bash
docker run -d \
  --name node-exporter \
  --restart unless-stopped \
  --net host \
  --pid host \
  -v /:/host:ro,rslave \
  quay.io/prometheus/node-exporter:latest \
  --path.rootfs=/host
```

### Option B: Systemd Service

```bash
# Download latest release
wget https://github.com/prometheus/node_exporter/releases/download/v1.8.2/node_exporter-1.8.2.linux-amd64.tar.gz
tar xvfz node_exporter-1.8.2.linux-amd64.tar.gz
sudo mv node_exporter-1.8.2.linux-amd64/node_exporter /usr/local/bin/

# Create systemd service
sudo tee /etc/systemd/system/node_exporter.service << 'EOF'
[Unit]
Description=Node Exporter
After=network.target

[Service]
User=node_exporter
ExecStart=/usr/local/bin/node_exporter
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo useradd -rs /bin/false node_exporter
sudo systemctl daemon-reload
sudo systemctl enable --now node_exporter
```

Node exporter runs on port `9100` by default.

## 2. Install NVIDIA DCGM Exporter (GPU Metrics)

DCGM exporter provides GPU utilization, temperature, power, and memory metrics.

### Option A: Docker (Recommended)

```bash
docker run -d \
  --name dcgm-exporter \
  --restart unless-stopped \
  --gpus all \
  --cap-add SYS_ADMIN \
  -p 9400:9400 \
  nvcr.io/nvidia/k8s/dcgm-exporter:3.3.8-3.6.0-ubuntu22.04
```

### Option B: Kubernetes DaemonSet

If running in Kubernetes with GPU nodes:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dcgm-exporter
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app: dcgm-exporter
  template:
    metadata:
      labels:
        app: dcgm-exporter
    spec:
      containers:
      - name: dcgm-exporter
        image: nvcr.io/nvidia/k8s/dcgm-exporter:3.3.8-3.6.0-ubuntu22.04
        ports:
        - containerPort: 9400
          name: metrics
        securityContext:
          capabilities:
            add: ["SYS_ADMIN"]
        resources:
          limits:
            nvidia.com/gpu: 1
      tolerations:
      - key: nvidia.com/gpu
        operator: Exists
        effect: NoSchedule
```

DCGM exporter runs on port `9400` by default.

## 3. Configure Prometheus

Add the following scrape configs to your `prometheus.yml`:

```yaml
scrape_configs:
  # Node Exporter - System metrics
  - job_name: 'node'
    static_configs:
      - targets: ['<your-server>:9100']
        labels:
          instance: '<your-server>'

  # DCGM Exporter - GPU metrics
  - job_name: 'dcgm'
    static_configs:
      - targets: ['<your-server>:9400']
        labels:
          instance: '<your-server>'

  # LLMKube Controller - Model/Service metrics (if running)
  - job_name: 'llmkube'
    static_configs:
      - targets: ['<your-server>:8080']
        labels:
          instance: '<your-server>'
```

Replace `<your-server>` with your server's hostname or IP address.

### Prometheus Docker Compose Example

```yaml
version: '3.8'
services:
  prometheus:
    image: prom/prometheus:latest
    container_name: prometheus
    restart: unless-stopped
    ports:
      - "9090:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus_data:/prometheus
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.path=/prometheus'
      - '--storage.tsdb.retention.time=30d'

volumes:
  prometheus_data:
```

## 4. Import Dashboard into Grafana

1. Open Grafana (default: http://localhost:3000)
2. Go to **Dashboards** > **Import**
3. Click **Upload JSON file**
4. Select `llmkube-gpu-dashboard.json`
5. Select your Prometheus datasource
6. Click **Import**

Alternatively, use the Grafana API:

```bash
curl -X POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d @llmkube-gpu-dashboard.json \
  http://localhost:3000/api/dashboards/db
```

## 5. Verify Setup

Check that metrics are being collected:

```bash
# Node exporter metrics
curl http://<your-server>:9100/metrics | grep node_cpu

# DCGM exporter metrics
curl http://<your-server>:9400/metrics | grep DCGM_FI_DEV_GPU

# Prometheus targets (should show UP)
curl http://prometheus:9090/api/v1/targets
```

## Available Metrics

### System Metrics (node_exporter)

| Metric | Description |
|--------|-------------|
| `node_cpu_seconds_total` | CPU time by mode |
| `node_memory_*` | Memory statistics |
| `node_filesystem_*` | Disk usage |
| `node_disk_*` | Disk I/O |
| `node_network_*` | Network traffic |
| `node_hwmon_temp_celsius` | Hardware temperatures |
| `node_boot_time_seconds` | System boot time |

### GPU Metrics (DCGM)

| Metric | Description |
|--------|-------------|
| `DCGM_FI_DEV_GPU_UTIL` | GPU utilization % |
| `DCGM_FI_DEV_GPU_TEMP` | GPU temperature (C) |
| `DCGM_FI_DEV_POWER_USAGE` | Power draw (W) |
| `DCGM_FI_DEV_FB_USED` | GPU memory used |
| `DCGM_FI_DEV_FB_FREE` | GPU memory free |
| `DCGM_FI_DEV_MEM_COPY_UTIL` | Memory copy utilization |

### LLMKube Metrics (if instrumented)

| Metric | Description |
|--------|-------------|
| `llmkube_model_status` | Model download status |
| `llmkube_inferenceservice_status` | Service running status |
| `llmkube_inference_requests_total` | Total inference requests |
| `llmkube_inference_latency_seconds` | Request latency histogram |

## Troubleshooting

### No GPU metrics showing

1. Verify NVIDIA drivers: `nvidia-smi`
2. Check DCGM exporter logs: `docker logs dcgm-exporter`
3. Ensure the container has GPU access: `docker run --gpus all nvidia/cuda:12.0-base nvidia-smi`

### No system temperature readings

Some systems require additional kernel modules:

```bash
sudo apt install lm-sensors
sudo sensors-detect  # Follow prompts
sudo systemctl restart node_exporter
```

### Prometheus not scraping

1. Check target status in Prometheus UI: `http://prometheus:9090/targets`
2. Verify network connectivity to exporters
3. Check firewall rules for ports 9100 and 9400
