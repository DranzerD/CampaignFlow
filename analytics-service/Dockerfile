FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/analytics-service ./cmd

FROM alpine:3.19
WORKDIR /app
COPY --from=build /out/analytics-service /app/analytics-service
COPY migrations /app/migrations
EXPOSE 8081
CMD ["/app/analytics-service"]
