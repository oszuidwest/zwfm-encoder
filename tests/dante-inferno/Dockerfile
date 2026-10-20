ARG INFERNO_IMAGE=zwfm-dante-audit-inferno-release:9767558
FROM ${INFERNO_IMAGE} AS inferno

FROM alpine:3.23 AS clock
RUN apk add --no-cache gcc musl-dev
COPY clock.c /build/clock.c
RUN gcc -O2 -o /build/test-clock /build/clock.c

FROM inferno
RUN apk add --no-cache alsa-utils ffmpeg python3
COPY --from=clock /build/test-clock /usr/local/bin/test-clock
WORKDIR /test
