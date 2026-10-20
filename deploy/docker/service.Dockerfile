# Generic build for the Go services in this repo.
# Build args: SERVICE selects services/<SERVICE>/cmd/<SERVICE>.
# The binary is copied as /bin/app; a gRPC health probe ships alongside it so
# compose healthchecks need no external tools.

ARG SERVICE=identity
FROM golang:1.24-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/app ./services/${SERVICE}/cmd/${SERVICE} \
 && CGO_ENABLED=0 go build -trimpath -o /out/grpc-health-probe ./scripts/grpc-health-probe

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/ /bin/
USER app
ENTRYPOINT ["/bin/app"]
