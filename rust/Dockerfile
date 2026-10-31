FROM rust:1 AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release

# aws-sdk uses rustls (no OpenSSL), so glibc-only distroless is enough.
FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /src/target/release/sftp-s3-rs /sftp-s3
ENV PORT=2222
EXPOSE 2222
ENTRYPOINT ["/sftp-s3"]
