#!/usr/bin/env bash
# Kafka topic initialization script
# Run after Kafka broker is healthy

set -euo pipefail

KAFKA_BROKER="${KAFKA_BROKER:-kafka:9092}"

TOPICS=(
    "rate-limit-events:12:1"         # Main event stream (partitions:replication)
    "anomaly-scores:6:1"             # ML sidecar anomaly score outputs
    "rate-limit-decisions:12:1"      # Decision engine verdicts
    "client-fingerprints:6:1"        # Enriched client fingerprint data
    "dlq-rate-limit-events:3:1"      # Dead-letter queue
)

echo "Waiting for Kafka to be ready..."
until kafka-topics --bootstrap-server "$KAFKA_BROKER" --list > /dev/null 2>&1; do
    sleep 2
done

for topic_spec in "${TOPICS[@]}"; do
    IFS=':' read -r topic partitions replication <<< "$topic_spec"
    echo "Creating topic: $topic (partitions=$partitions, replication=$replication)"
    kafka-topics --bootstrap-server "$KAFKA_BROKER" \
        --create \
        --if-not-exists \
        --topic "$topic" \
        --partitions "$partitions" \
        --replication-factor "$replication"
done

echo "All topics created successfully."
