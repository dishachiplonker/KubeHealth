FROM golang:alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/kubehealth .

# ---- runtime stage ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /out/kubehealth /usr/local/bin/kubehealth

RUN adduser -D -H -u 10001 appuser
USER 10001

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kubehealth"]
