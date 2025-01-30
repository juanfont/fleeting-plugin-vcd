build:
	goreleaser 

test:
	go test -v -timeout 3600s -count=1 ./...

test-integration:
	@if [ -z "$(test)" ]; then \
		echo "Usage: make test-integration test=TestName"; \
		echo "Example: make test-integration test=TestProvisioning/static_credentials_via_ssh_keys"; \
		exit 1; \
	fi
	go test -v -timeout 3600s -count=1 ./... -run $(test)
