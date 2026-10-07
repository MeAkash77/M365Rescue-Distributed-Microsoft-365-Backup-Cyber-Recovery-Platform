FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
# go.sum is resolved here so the repo works with only `docker compose up --build`
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/m365rescue ./cmd/m365rescue

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
COPY --from=build /out/m365rescue /usr/local/bin/m365rescue
USER app
EXPOSE 8080 9100
ENTRYPOINT ["m365rescue"]
