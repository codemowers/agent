FROM golang:1.27.3-trixie AS build
WORKDIR /app
COPY go.mod go.sum* ./
ENV GOFLAGS=-mod=mod GONOSUMDB=*
RUN go mod download
COPY *.go ./
COPY api/ ./api/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /operator .

FROM scratch AS runtime
COPY --from=build /operator /operator
ENTRYPOINT ["/operator"]
