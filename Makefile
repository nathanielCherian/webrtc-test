.PHONY: build run standin docker docker-mac clean

build:
	CGO_ENABLED=1 go build -o bin/server ./cmd/server

standin: media/standin.mp4

media/standin.mp4:
	./scripts/make-standin.sh $@

run: build standin
	./bin/server $(ARGS)

docker:
	docker compose up --build

docker-mac:
	docker compose -f docker-compose.yml -f docker-compose.mac.yml up --build

clean:
	rm -rf bin
