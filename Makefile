.PHONY: build owdserver run standin docker docker-mac clean

build:
	CGO_ENABLED=1 go build -o bin/server ./cmd/server
	go build -o bin/simclient ./cmd/simclient
	go build -o bin/udptunnel ./cmd/udptunnel
	$(MAKE) owdserver

# Separate Go module (no cgo), so it can be built and deployed on its own.
owdserver:
	cd owdserver && CGO_ENABLED=0 go build -o ../bin/owdserver .

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
