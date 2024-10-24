FROM golang:1.22.5 AS builder
WORKDIR /app
ADD cmd cmd
ADD internal internal
ADD pkg pkg
ADD go.mod go.mod
ADD go.sum go.sum
RUN CGO_ENABLED=0 go build cmd/operator/operator.go

FROM gcr.io/distroless/static-debian12:nonroot AS final
COPY --from=builder /app/operator /operator
ENTRYPOINT ["/operator"]
CMD ["run"]