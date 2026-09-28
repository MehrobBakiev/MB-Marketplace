FROM golang:1.23-alpine

WORKDIR /app

COPY . .

RUN go build -o service ./cmd

EXPOSE 8080

CMD ["./service"]
