FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o ./anyAIProxyAPI ./main.go

FROM eceasy/fingerprint-chromium:130.0.6723.116

RUN mkdir /anyAIProxyAPI

COPY --from=builder ./app/anyAIProxyAPI /anyAIProxyAPI/anyAIProxyAPI

WORKDIR /anyAIProxyAPI

EXPOSE 2048

CMD ["./anyAIProxyAPI"]