FROM golang:1.23
WORKDIR /src
COPY . .
RUN go build -o /app ./...
CMD ["/app"]
