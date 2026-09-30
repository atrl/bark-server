# Build the static linux/amd64 binary with build.sh before building this image.
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8

ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/atrl/bark-server" \
      org.opencontainers.image.revision="${REVISION}"

RUN test -s /etc/ssl/cert.pem && mkdir /data && chown 65532:65532 /data
COPY bark-server /usr/local/bin/bark-server
RUN chmod 0555 /usr/local/bin/bark-server
USER 65532:65532
VOLUME /data
ENTRYPOINT ["/usr/local/bin/bark-server"]
