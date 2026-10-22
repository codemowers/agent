FROM golang:1.27.3-trixie AS build
WORKDIR /app
COPY go.mod go.sum ./
ENV GOFLAGS=-mod=mod
RUN go mod download
COPY api/ ./api/
COPY feeds/ ./feeds/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /feeds ./feeds

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /feeds /feeds
ENTRYPOINT ["/feeds"]
