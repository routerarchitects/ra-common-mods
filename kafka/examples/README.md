# Kafka Module Example

This example demonstrates the usage of the Kafka module with:
- **10 Producer Goroutines** - Publishing messages concurrently
- **10 Consumer Workers** - Processing messages in parallel

## Prerequisites

1. **Kafka running locally**:
   ```bash
   docker run -d --name kafka \
     -p 9092:9092 \
     -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092 \
     apache/kafka:latest
   ```

2. **Create the topic**:
   ```bash
   docker exec -it kafka /opt/kafka/bin/kafka-topics.sh \
     --create \
     --topic example-topic \
     --bootstrap-server localhost:9092 \
     --partitions 10 \
     --replication-factor 1
   ```

## Running the Example

```bash
cd examples
go run main.go
```

## What It Does

### Producer Side
- Spawns **10 goroutines** (producer workers)
- Each worker publishes 1 message per second
- Messages are JSON-encoded events with unique IDs
- Total throughput: ~10 messages/second

### Consumer Side
- Uses **10 concurrent workers** via `SubscribeOptions`
- Processes messages with simulated processing time
- Tracks statistics by producer worker
- Reports stats every 10 seconds

## Expected Output

```
{"level":"INFO","msg":"Producer started"}
{"level":"INFO","msg":"Consumer started with 10 workers"}
{"level":"INFO","msg":"Published message","worker":1,"event_id":"event-1-0"}
{"level":"INFO","msg":"Published message","worker":2,"event_id":"event-2-0"}
{"level":"INFO","msg":"Processed message","event_id":"event-1-0","total_processed":1}
{"level":"INFO","msg":"Processed message","event_id":"event-2-0","total_processed":2}
...
{"level":"INFO","msg":"=== Consumer Statistics ===","total_messages_processed":100}
```

## Key Features Demonstrated

1. **Concurrent Producers**: Multiple goroutines publishing independently
2. **Worker Pool**: Consumer with 10 workers processing in parallel
3. **Graceful Shutdown**: Handles SIGINT/SIGTERM signals
4. **Statistics**: Tracks and reports processing metrics
5. **Error Handling**: Retry policy with exponential backoff
6. **Middleware**: Recovery and metrics interceptors

## Stopping the Example

Press `Ctrl+C` to trigger graceful shutdown. The example will:
1. Stop producing new messages
2. Finish processing remaining messages
3. Print final statistics
4. Clean up resources

## Cleanup

```bash
docker stop kafka
docker rm kafka
```
