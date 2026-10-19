FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmcache ./cmd/llmcache && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakeupstream ./cmd/fakeupstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
EXPOSE 8080
CMD ["/llmcache"]
