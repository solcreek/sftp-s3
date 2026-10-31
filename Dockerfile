FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sftp-s3 .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sftp-s3 /sftp-s3
ENV PORT=2222
EXPOSE 2222
ENTRYPOINT ["/sftp-s3"]
