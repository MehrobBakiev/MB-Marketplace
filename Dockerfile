FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go generate ./api && CGO_ENABLED=0 go build -trimpath -o /marketplace ./cmd

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app
COPY --from=build /marketplace /usr/local/bin/marketplace
EXPOSE 8080
ENTRYPOINT ["marketplace"]
