FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /token-svc .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /token-svc /token-svc
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/token-svc"]
