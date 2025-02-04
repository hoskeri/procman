build:
	go build -v -o ./procman ./cmd/procman
	go build -v -o ./tools/trebuchet/trebuchet ./tools/trebuchet

test:
	go test -v ./...

clean:
	rm -v -f ./procman

.PHONY: build clean
