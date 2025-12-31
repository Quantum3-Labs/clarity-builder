FROM golang:1.24 AS go-builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

RUN go install github.com/swaggo/swag/cmd/swag@latest

COPY . ./

RUN swag init -g cmd/server/main.go -o docs

RUN CGO_ENABLED=1 GOOS=linux go build -o server ./cmd/server/main.go

FROM python:3.11-slim

WORKDIR /app

RUN apt-get update && apt-get install -y \
    git \
    wget \
    awscli \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /build/server /app/server
COPY --from=go-builder /build/docs /app/docs

COPY requirements.txt /app/requirements.txt

# Install PyTorch CPU-only version
RUN pip install --no-cache-dir \
    torch --index-url https://download.pytorch.org/whl/cpu

RUN pip install --no-cache-dir -r requirements.txt

COPY scripts/ /app/scripts/

RUN mkdir -p /app/data/chromadb /app/data/clarity_code_samples /app/data/clarity_official_docs

ENV PORT=8080
ENV GIN_MODE=release
ENV PYTHONUNBUFFERED=1

EXPOSE 8080

VOLUME ["/app/data"]

COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

ENTRYPOINT ["/app/entrypoint.sh"]
