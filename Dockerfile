# Consumed by GoReleaser: it copies the already cross-compiled binary out of the
# build context rather than compiling, so the image build is fast and ships the
# same static binary as every other artifact.
#
# doc is a pure-Go CLI with no runtime dependencies, so the image only needs
# ca-certificates and tzdata.
#
# GoReleaser builds one multi-platform image with buildx and stages each
# platform's binary under a $TARGETPLATFORM directory (e.g. linux/amd64/) in the
# build context, so the COPY line selects the right one through the automatic
# TARGETPLATFORM build arg.
FROM alpine:3.21

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata \
 && mkdir -p /data

COPY $TARGETPLATFORM/doc /usr/bin/doc

WORKDIR /data

# Database files live under the mounted /data volume by default:
#
#   docker run -v "$PWD:/data" ghcr.io/tamnd/doc app.doc --eval 'db.users.count()'
#
# The wire server listens on 27017; publish it to connect a MongoDB driver:
#
#   docker run -p 27017:27017 -v "$PWD:/data" ghcr.io/tamnd/doc app.doc serve --bind 0.0.0.0
#
VOLUME ["/data"]
EXPOSE 27017

ENTRYPOINT ["/usr/bin/doc"]
