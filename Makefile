BINARY := axis_airquality
CMD := ./cmd/axis_airquality

.PHONY: all build test vet fmt clean tidy

all: build

build:
	go build -o $(BINARY) $(CMD)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
