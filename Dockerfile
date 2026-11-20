# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
# 版本号由构建方注入，保证 /healthz 能自证跑了哪个版本
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/officeace2api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/officeace2api .
VOLUME ["/app/data"]
EXPOSE 8787
ENV TZ=Asia/Shanghai
ENTRYPOINT ["./officeace2api"]
CMD ["-data","/app/data","-listen","0.0.0.0:8787"]
