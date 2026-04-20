build:
  go build -o bin/pgpool ./cmd/pgpool
  go build -o bin/pgpoolcli ./cmd/pgpoolcli

pgpool:
  go run ./cmd/pgpool/pgpool.go --pg-password password
cli *ARGS:
  go run ./cmd/pgpoolcli {{ARGS}}
