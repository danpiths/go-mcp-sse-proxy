#!/bin/bash
set -e

echo "🔄 Starting deployment process..."

# Function to check if a machine exists
check_machine_exists() {
    fly machine list | grep -q "docker-daemon"
}

# Function to get machine ID
get_machine_id() {
    fly machine list | grep "docker-daemon" | awk '{print $1}'
}

# Function to wait for DinD to be ready
wait_for_dind() {
    echo "⏳ Waiting for DinD to be ready..."
    local max_attempts=30
    local attempt=1

    # Get the machine ID
    local machine_id=$(get_machine_id)
    if [ -z "$machine_id" ]; then
        echo "❌ Failed to get machine ID"
        exit 1
    fi

    while [ $attempt -le $max_attempts ]; do
        if fly machine status "$machine_id" | grep -q "started"; then
            echo "✅ DinD is ready!"
            return 0
        fi
        echo "⏳ Attempt $attempt/$max_attempts: DinD not ready yet, waiting..."
        sleep 10
        ((attempt++))
    done

    echo "❌ DinD failed to start within timeout"
    exit 1
}

# Step 1: Handle existing DinD container
if check_machine_exists; then
    machine_id=$(get_machine_id)
    if [ -n "$machine_id" ]; then
        echo "🛑 Stopping existing DinD container..."
        fly machine stop "$machine_id"
        echo "Waiting for machine to stop..."
        sleep 10

        echo "🗑️ Destroying existing DinD container..."
        fly machine destroy "$machine_id"
        # Wait a bit for the destruction to complete
        sleep 10
    fi
fi

# Step 2: Deploy new DinD container
echo "🐳 Deploying new DinD container..."
fly machine run docker:dind \
    --name docker-daemon \
    --region bom \
    --env DOCKER_TLS_CERTDIR="" \
    --port 2375/tcp \
    --vm-cpu-kind shared \
    --vm-cpus 1 \
    --vm-memory 2048

# Wait for DinD to be ready
wait_for_dind

# Step 3: Retrieve DIND machine ID and set DOCKER_HOST
machine_id=$(get_machine_id)

# Step 4: Deploy the proxy application with DOCKER_HOST
echo "🚀 Deploying proxy application..."
fly deploy --env DOCKER_HOST=tcp://$machine_id.vm.go-mcp-sse-proxy.internal:2375 --ha=false

echo "✨ Deployment completed successfully!"
