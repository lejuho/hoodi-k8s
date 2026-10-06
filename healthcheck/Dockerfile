FROM golang:1 AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/healthcheck .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/healthcheck /healthcheck
USER nonroot:nonroot
ENTRYPOINT ["/healthcheck"]
