.PHONY: verify vet bench race fmt

verify:
	go test ./... -count=1 -v

vet:
	go vet ./...

fmt:
	gofmt -w .

bench:
	go test -run '^$$' -bench Replay30k -benchtime=3x ./engine

race:
	CGO_ENABLED=1 go test -race -count=1 ./...
