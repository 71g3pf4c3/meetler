FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /token-svc .
# Каталоги для волюмов (soundboard/аватарки): named-волюм при ПЕРВОМ маунте
# наследует владение из образа (docker copy-on-first-use). Без этого волюм
# будет root:root, а контейнер работает как distroless nonroot (65532)
# → os.Create получит EACCES.
RUN mkdir -p /data/sounds /data/avatars && chown -R 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /token-svc /token-svc
COPY --from=build --chown=65532:65532 /data /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/token-svc"]
