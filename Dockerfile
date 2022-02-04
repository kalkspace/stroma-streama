FROM golang:1.17-alpine AS build

RUN apk add pkgconfig gcc musl-dev opus-dev opusfile-dev portaudio-dev

COPY . /src
WORKDIR /src
RUN go build main.go


FROM alpine

RUN apk add opus opusfile portaudio pulseaudio pulseaudio-alsa alsa-plugins-pulse alsa-utils

COPY --from=build /src/main /usr/local/bin/stroma-streama
COPY contrib/pulse-client.conf /etc/pulse/client.conf

ENTRYPOINT ["/usr/local/bin/stroma-streama"]
