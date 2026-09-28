FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /wallet ./cmd/wallet

FROM gcr.io/distroless/static-debian12
COPY --from=build /wallet /wallet
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/wallet"]
