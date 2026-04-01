# build method: just run `docker build --rm -t rep1ace/wssocks .`

# build frontend code
FROM node:14.15.4-alpine3.12 AS web-builder

COPY status-web web/

RUN cd web \
    && yarn install \
    && yarn build

## build go binary
FROM golang:1.15.7-alpine3.13 AS builder

ARG PACKAGE=github.com/rep1ace/wssocks
ARG BUILD_FLAG="-X 'github.com/rep1ace/wssocks/version.buildHash=`git rev-parse HEAD`' \
 -X 'github.com/rep1ace/wssocks/version.buildTime=`date`' \
 -X 'github.com/rep1ace/wssocks/version.buildGoVersion=`go version | cut -f 3,4 -d\" \"`'"

RUN apk add --no-cache git \
    && go get -u github.com/rakyll/statik

COPY ./  /go/src/${PACKAGE}/
COPY --from=web-builder web/build /go/src/github.com/rep1ace/wssocks/web-build/

RUN cd ./src/${PACKAGE} \
    && cd cmd/server \
    && statik -src=../../web-build \
    && cd ../../ \
    && go build -ldflags "${BUILD_FLAG}" -o wssocks ${PACKAGE} \
    && go install

## copy binary
FROM alpine:3.13.0

ARG HOME="/home/wssocks"

RUN adduser -D wssocks -h ${HOME}

COPY --from=builder --chown=wssocks /go/bin/wssocks ${HOME}/wssocks

WORKDIR ${HOME}
USER wssocks

ENTRYPOINT ["./wssocks"]
CMD ["--help"]
