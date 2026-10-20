FROM golang:1.27.3-trixie AS build
WORKDIR /app
COPY go.mod go.sum ./
ENV GOFLAGS=-mod=mod
RUN go mod download
COPY api/ ./api/
COPY devicebridge/ ./devicebridge/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /devicebridge ./devicebridge

FROM scratch
COPY --from=build /devicebridge /devicebridge
ENTRYPOINT ["/devicebridge"]
