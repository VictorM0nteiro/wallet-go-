FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /wallet ./cmd/wallet
RUN CGO_ENABLED=0 go build -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12 AS wallet
COPY --from=build /wallet /wallet
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/wallet"]

FROM gcr.io/distroless/static-debian12 AS relay
COPY --from=build /relay /relay
USER nonroot:nonroot
EXPOSE 9091
ENTRYPOINT ["/relay"]
