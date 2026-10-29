FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BIN
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
