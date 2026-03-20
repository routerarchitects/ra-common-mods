# Kafka Module Examples

This folder contains two runnable examples:
- `basic`: simple producer + consumer flow
- `advanced`: retry/DLQ/concurrency/multi-consumer scenarios

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

## Running the Examples

```bash
cd examples
go run ./basic
go run ./advanced
```

## Basic Example

### Producer Side
- Spawns producer goroutine(s) (currently configured as 1 worker in code)
- Worker publishes 1 message per second
- Messages are JSON-encoded events with unique IDs
- Throughput depends on configured worker count

### Consumer Side
- Consumes messages and processes them synchronously per claim
- Processes messages with simulated processing time
- Tracks statistics by producer worker
- Reports stats every 10 seconds

## Expected Output

```
{"level":"INFO","msg":"Producer started"}
{"level":"INFO","msg":"Consumer started"}
{"level":"INFO","msg":"Published message","worker":1,"event_id":"event-1-0"}
{"level":"INFO","msg":"Processed message","event_id":"event-1-0","total_processed":1}
...
{"level":"INFO","msg":"=== Consumer Statistics ===","total_messages_processed":20}
```

## Key Features Demonstrated

1. **Concurrent Producers**: Multiple goroutines publishing independently
2. **Retry Policy**: Exponential backoff using `RetryPolicy`
3. **Graceful Shutdown**: Handles SIGINT/SIGTERM signals
4. **Statistics**: Tracks and reports processing metrics
5. **Error Handling**: Logs publish/consume failures
6. **Middleware**: Recovery and metrics interceptors

## Advanced Example Scenarios

1. Retry with eventual success
2. DLQ flow verification
3. Concurrency with multiple consumers in same group
4. Independent consumers on different topics

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
