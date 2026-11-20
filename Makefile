BIN := officeace2api

.PHONY: build run vet clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) .

run: build
	./$(BIN) -data data

vet:
	go vet ./...

clean:
	rm -f $(BIN)
