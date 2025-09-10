#!/bin/bash

echo "🧪 Carbon-Aware Autoscaler Test Script"
echo "======================================"

# Function to show current status
show_status() {
    echo ""
    echo "📊 Current Status:"
    echo "Deployments:"
    kubectl get deployments -o custom-columns="NAME:.metadata.name,REPLICAS:.status.replicas,READY:.status.readyReplicas"
    echo ""
    echo "Pods by Node:"
    kubectl get pods -o wide --sort-by='.spec.nodeName'
    echo ""
}

# Function to create CPU load
create_cpu_load() {
    echo "🔥 Creating CPU load to trigger scaling..."
    kubectl run cpu-loader-1 --image=busybox --restart=Never -- /bin/sh -c 'while true; do :; done' &
    kubectl run cpu-loader-2 --image=busybox --restart=Never -- /bin/sh -c 'while true; do :; done' &
    kubectl run cpu-loader-3 --image=busybox --restart=Never -- /bin/sh -c 'while true; do :; done' &
    echo "CPU load pods created. Waiting for them to start..."
    sleep 10
}

# Function to remove CPU load
remove_cpu_load() {
    echo "🧹 Removing CPU load..."
    kubectl delete pod cpu-loader-1 --ignore-not-found=true
    kubectl delete pod cpu-loader-2 --ignore-not-found=true
