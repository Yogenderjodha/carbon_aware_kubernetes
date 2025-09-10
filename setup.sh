#!/bin/bash
set -e

echo "🌍 Carbon-Aware Autoscaler Setup Script"
echo "========================================"

# Check if we're in the right directory
if [[ ! -f "main.go" ]]; then
    echo "❌ Error: main.go not found. Please run this script from the project directory."
    exit 1
fi

# Step 1: Check if kind cluster exists
echo "1️⃣ Checking kind cluster..."
if ! kind get clusters | grep -q "carbon-aware"; then
    echo "Creating kind cluster..."
    cat <<EOL | kind create cluster --name carbon-aware --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
- role: worker
EOL
else
    echo "✅ Kind cluster 'carbon-aware' already exists"
fi

# Step 2: Install metrics-server
echo ""
echo "2️⃣ Installing metrics-server..."
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# Patch metrics-server for kind (insecure TLS)
kubectl patch deployment metrics-server -n kube-system --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--kubelet-insecure-tls"
  }
]'

echo "Waiting for metrics-server to be ready..."
kubectl wait --for=condition=ready pod -l k8s-app=metrics-server -n kube-system --timeout=120s

# Step 3: Initialize Go modules
echo ""
echo "3️⃣ Initializing Go modules..."
go mod tidy

# Step 4: Build and load Docker image
echo ""
echo "4️⃣ Building Docker image..."
docker build -t k8s-autoscaler:local .

echo "Loading image into kind cluster..."
kind load docker-image k8s-autoscaler:local --name carbon-aware

# Step 5: Apply RBAC
echo ""
echo "5️⃣ Applying RBAC configuration..."
kubectl apply -f autoscaler-rbac.yaml

# Step 6: Apply secrets
echo ""
echo "6️⃣ Applying secrets..."
kubectl apply -f electricitymap-secret.yaml

# Step 7: Deploy nginx workloads
echo ""
echo "7️⃣ Deploying nginx workloads..."
kubectl apply -f nginx-country-deployments.yaml

# Step 8: Deploy autoscaler
echo ""
echo "8️⃣ Deploying autoscaler..."
kubectl apply -f autoscaler-deployment.yaml

# Step 9: Wait for deployments
echo ""
echo "9️⃣ Waiting for deployments to be ready..."
kubectl wait --for=condition=available deployment/nginx-denmark --timeout=120s
kubectl wait --for=condition=available deployment/nginx-france --timeout=120s
kubectl wait --for=condition=available deployment/nginx-latvia --timeout=120s
kubectl wait --for=condition=available deployment/k8s-autoscaler --timeout=120s

# Step 10: Show status
echo ""
echo "✅ Setup complete! Here's the current status:"
echo ""
echo "Nodes:"
kubectl get nodes
echo ""
echo "Deployments:"
kubectl get deployments
echo ""
echo "Pods:"
kubectl get pods -o wide
echo ""
echo "📊 To monitor the autoscaler:"
echo "kubectl logs -f deployment/k8s-autoscaler"
echo ""
echo "📈 To check metrics:"
echo "kubectl top nodes"
echo "kubectl top pods"