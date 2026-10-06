# Used by GoReleaser, which supplies the prebuilt binary for each platform.
# The image doubles as the runner job's prestart image (narc-image-fetch).
FROM alpine:3.22
RUN apk add --no-cache ca-certificates curl qemu-img
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/narc /usr/local/bin/narc
COPY scripts/narc-image-fetch /usr/local/bin/narc-image-fetch
USER 65534
ENTRYPOINT ["/usr/local/bin/narc"]
