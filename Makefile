build:
	go test -v ./...
	go build -v ./...
	go build -v -o ./procman ./cmd/procman

clean:
	rm -v -f ./procman

.PHONY: build clean
