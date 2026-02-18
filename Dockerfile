FROM golang:1.25 AS dev
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download
RUN go install github.com/air-verse/air@latest

COPY . .

ENV NAMESPACE=default
ENV TEMPLATE_PATH=/workspace/config/template/resources.yaml
ENV VALUES_PATH=/workspace/config/template/values.yaml
ENV API_ADDR=:8080
ENV METRICS_ADDR=:8081
ENV PROBE_ADDR=:8082
ENV RECONCILE_INTERVAL=30s

EXPOSE 8080 8081 8082
ENTRYPOINT ["air", "-c", ".air.toml"]

FROM golang:1.25 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/server ./cmd/server

FROM gcr.io/distroless/static:nonroot AS prod
WORKDIR /app

COPY --from=builder /out/server /app/server
COPY config/template/resources.yaml /app/config/template/resources.yaml
COPY config/template/values.yaml /app/config/template/values.yaml

ENV NAMESPACE=default
ENV TEMPLATE_PATH=/app/config/template/resources.yaml
ENV VALUES_PATH=/app/config/template/values.yaml
ENV API_ADDR=:8080
ENV METRICS_ADDR=:8081
ENV PROBE_ADDR=:8082
ENV DEFAULT_TTL=10m
ENV RECONCILE_INTERVAL=30s

EXPOSE 8080 8081 8082
ENTRYPOINT ["/app/server"]