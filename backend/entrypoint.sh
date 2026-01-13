#!/bin/bash
set -e

# Configuration
S3_DATA_PATH="${S3_DATA_PATH:-s3://quantum3labs/stacks-builder-data}"
LOCAL_DATA_PATH="${DATA_DIR:-/app/data}"
DB_FILE="$LOCAL_DATA_PATH/clarity_coder.db"
SYNC_INTERVAL="${SYNC_INTERVAL:-300}"  # 5 minutes

log() {
    echo "[entrypoint] $(date '+%Y-%m-%d %H:%M:%S') $1"
}

# Sync local data to S3
sync_to_s3() {
    log "Syncing data to S3..."

    # Exclude WAL files (merged via checkpoint), logs, and temp files
    aws s3 sync "$LOCAL_DATA_PATH" "$S3_DATA_PATH" \
        --exclude "*.db-wal" \
        --exclude "*.db-shm" \
        --exclude "*.log" \
        --exclude "*.tmp" \
        --exclude "__pycache__/*" \
        --quiet

    log "S3 sync complete"
}

# Sync SQLite only (for periodic sync)
sync_db_to_s3() {
    if [ -f "$DB_FILE" ]; then
        aws s3 cp "$DB_FILE" "$S3_DATA_PATH/clarity_coder.db" --quiet 2>/dev/null || true
    fi
}

# Graceful shutdown handler
shutdown_handler() {
    log "Shutdown signal received"

    # Stop periodic sync
    if [ -n "$SYNC_PID" ]; then
        kill "$SYNC_PID" 2>/dev/null || true
    fi

    # Send SIGTERM to server (triggers graceful shutdown + SQLite checkpoint)
    if [ -n "$SERVER_PID" ]; then
        log "Stopping server (PID: $SERVER_PID)..."
        kill -TERM "$SERVER_PID" 2>/dev/null || true

        # Wait for server to exit gracefully (max 30 seconds)
        for i in $(seq 1 30); do
            if ! kill -0 "$SERVER_PID" 2>/dev/null; then
                break
            fi
            sleep 1
        done

        # Force kill if still running
        kill -9 "$SERVER_PID" 2>/dev/null || true
    fi

    # Final sync to S3
    sync_to_s3

    log "Shutdown complete"
    exit 0
}

# Trap shutdown signals
trap shutdown_handler SIGTERM SIGINT

# Create data directory
mkdir -p "$LOCAL_DATA_PATH"

# Download existing data from S3
log "Checking S3 for existing data..."
if aws s3 ls "$S3_DATA_PATH/clarity_coder.db" 2>/dev/null; then
    log "Found existing data in S3, downloading..."

    # Download all data except WAL files
    # On failure: delete partial data and start fresh to avoid corruption
    if aws s3 sync "$S3_DATA_PATH" "$LOCAL_DATA_PATH" \
        --exclude "*.db-wal" \
        --exclude "*.db-shm" \
        --quiet; then
        DATA_SIZE=$(du -sh "$LOCAL_DATA_PATH" 2>/dev/null | cut -f1)
        log "Downloaded $DATA_SIZE of data"
    else
        log "ERROR: S3 sync failed, clearing partial data and starting fresh"
        rm -rf "${LOCAL_DATA_PATH:?}"/*
    fi
else
    log "No existing data in S3, will initialize fresh"
fi

# Start the server in background
log "Starting server..."
/app/server &
SERVER_PID=$!
log "Server started (PID: $SERVER_PID)"

# Periodic sync loop (runs in background)
periodic_sync() {
    while true; do
        sleep "$SYNC_INTERVAL"
        log "Periodic sync..."
        sync_db_to_s3
    done
}

periodic_sync &
SYNC_PID=$!
log "Periodic sync started (PID: $SYNC_PID, interval: ${SYNC_INTERVAL}s)"

# Wait for server process
wait "$SERVER_PID"
SERVER_EXIT_CODE=$?

log "Server exited with code $SERVER_EXIT_CODE"

# If server exited unexpectedly, still try to sync
if [ $SERVER_EXIT_CODE -ne 0 ]; then
    log "Server crashed, attempting final sync..."
    sync_to_s3
fi

exit $SERVER_EXIT_CODE
