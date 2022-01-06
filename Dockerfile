FROM golang:1.17-alpine AS build

RUN apk add pkgconfig gcc musl-dev opus-dev opusfile-dev portaudio-dev

COPY . /src
WORKDIR /src
RUN go build main.go


FROM alpine

RUN apk add opus opusfile portaudio

COPY --from=build /src/main /usr/local/bin/stroma-streama

ENTRYPOINT ["/usr/local/bin/stroma-streama"]
