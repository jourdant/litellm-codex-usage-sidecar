FROM golang:1.24.5-alpine3.22@sha256:daae04ebad0c21149979cd8e9db38f565ecefd8547cf4a591240dc1972cf1399 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY providers/ ./providers/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/litellm-subscription-usage-sidecar .

FROM scratch
COPY --from=build /out/litellm-subscription-usage-sidecar /litellm-subscription-usage-sidecar
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/litellm-subscription-usage-sidecar"]
