isuports: test go.mod go.sum *.go cmd/isuports/*
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o isuports ./cmd/isuports

test:
	go test -v ./...
