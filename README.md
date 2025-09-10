# Carbon-Aware Kubernetes Scheduler


A secondary Kubernetes scheduler that prioritizes nodes in regions with **lower real-time carbon intensity**, while respecting standard constraints via the Scheduler Framework.


## Features
- **Score plugin** that boosts nodes with lower carbon intensity (gCO₂/kWh) from ElectricityMap.
- **Configurable** normalization ranges and weights via SchedulerConfiguration.
- **Caching + TTL** to avoid rate limits.
- **Kind multi-node** cluster config for local validation.
- **Validation workloads** and simple emissions estimator.


## Prerequisites
- Go 1.22+
- Docker/Podman
- kubectl, kind
- ElectricityMap API key (set `ELECTRICITYMAP_API_TOKEN`)


## Quick Start
```bash
# 1) Build image
IMAGE=ghcr.io/you/carbon-scheduler:dev
DOCKER_BUILDKIT=1 docker build -t $IMAGE .


# 2) Create Kind cluster
kind create cluster --config cluster/kind-config.yaml


# 3) Load image into Kind
kind load docker-image $IMAGE


# 4) Label nodes with ElectricityMap zones
bash cluster/label-nodes.sh


# 5) Deploy RBAC + Scheduler + Config
kubectl apply -f config/carbon-scheduler-rbac.yaml
kubectl -n kube-system create configmap carbon-scheduler-config --from-file=config/scheduler-config.yaml
kubectl -n kube-system apply -f deploy/deployment.yaml


# 6) Schedule sample workloads using this scheduler
kubectl apply -f workloads/cpu-benchmark.yaml
kubectl apply -f workloads/web-demo.yaml


# 7) Evaluate
bash evaluation/collect-metrics.sh
python3 evaluation/estimate-emissions.py