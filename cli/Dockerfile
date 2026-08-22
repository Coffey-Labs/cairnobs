# docker build -f cli/Dockerfile -t cairnobsctl cli/
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/cairnobsctl ./cmd/cairnobsctl

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/cairnobsctl /cairnobsctl
ENTRYPOINT ["/cairnobsctl"]
