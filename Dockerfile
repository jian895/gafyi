FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gafyi .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -H gafyi && mkdir -p /data && chown gafyi /data
COPY --from=build /gafyi /usr/local/bin/gafyi
USER gafyi
VOLUME /data
EXPOSE 8787
# Mount your config at /config.json with "listen": "0.0.0.0:8787" and "data_dir": "/data"
ENTRYPOINT ["gafyi", "-config", "/config.json"]
