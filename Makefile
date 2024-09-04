
build:
	goreleaser 

test:
	go test -v -timeout 3600s -count=1 ./...