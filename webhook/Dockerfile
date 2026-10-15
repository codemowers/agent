FROM golang:1.23-bookworm AS build
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /webhook .

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /webhook /webhook
EXPOSE 8080
ENTRYPOINT ["/webhook"]
