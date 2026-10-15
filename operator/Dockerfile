FROM golang:1.23-bookworm AS build
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY *.go ./
COPY api/ ./api/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /operator .

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /operator /operator
ENTRYPOINT ["/operator"]
