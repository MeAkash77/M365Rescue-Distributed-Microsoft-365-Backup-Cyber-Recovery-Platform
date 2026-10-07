.PHONY: up down logs test tidy scale demo clean
up:      ; docker compose up --build -d && echo "Open http://localhost:8080  (API key: dev-admin-key)"
down:    ; docker compose down
clean:   ; docker compose down -v
logs:    ; docker compose logs -f api worker
tidy:    ; go mod tidy
test:    ; go mod tidy && go vet ./... && go test ./...
scale:   ; docker compose up -d --scale worker=5
demo:    ; ./scripts/demo.sh
