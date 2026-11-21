# The image holds the released binary and nothing else to run: the workflow
# builds dist/shackle-linux-<arch> once (scripts/build.sh) and this file copies
# it in, so the binary in the image is byte for byte the one on the release
# page. To build the image by hand, run scripts/build.sh first.
#
# distroless/static brings the CA certificates the cloud APIs need and a
# non-root user; there is no shell and no package manager.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETARCH
COPY dist/shackle-linux-${TARGETARCH} /usr/local/bin/shackle
ENTRYPOINT ["/usr/local/bin/shackle"]
