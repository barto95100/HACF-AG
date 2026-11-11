# Image minimale : binaire Go statique, aucun shell, utilisateur non-root (10001).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ag-hacf .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ag-hacf /ag-hacf
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/ag-hacf"]
