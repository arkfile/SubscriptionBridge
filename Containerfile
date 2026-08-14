FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/bridge ./cmd/bridge \
 && CGO_ENABLED=0 go build -trimpath -o /out/bridge-cli ./cmd/bridge-cli

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/bridge /bridge
COPY --from=build /out/bridge-cli /bridge-cli
USER nonroot
ENTRYPOINT ["/bridge"]
